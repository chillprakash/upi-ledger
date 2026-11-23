package acp

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chillprakash/upi-ledger/internal/clock"
)

// Error is ACP's flat error object (no envelope).
type Error struct {
	Type    string `json:"type"` // invalid_request | processing_error | service_unavailable
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   string `json:"param,omitempty"` // RFC 9535 JSONPath
	// SupportedVersions is set on API-Version rejections (ACP 2.1).
	SupportedVersions []string `json:"supported_versions,omitempty"`
}

// WriteError writes an ACP error.
func WriteError(w http.ResponseWriter, status int, e Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(e) //nolint:errcheck
}

// WriteJSON writes a JSON body.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

// Policy configures the common ACP request rules.
type Policy struct {
	Versions []string // supported API-Version values
	// Tokens maps bearer tokens to the caller identity idempotency keys are
	// scoped to (ACP 6.1).
	Tokens map[string]string
	// SignerKeys, when set, requires a valid Signature + Timestamp header:
	// Ed25519 over Timestamp + "." + canonical body, from the caller's key.
	SignerKeys map[string]ed25519.PublicKey // identity -> key
	MaxSkew    time.Duration
	Clock      clock.Clock
	Retention  time.Duration // idempotency retention, at least 24 h (ACP 6.6)
}

type idemEntry struct {
	fingerprint string
	inFlight    bool
	status      int
	body        []byte
	header      http.Header
	at          time.Time
}

// Middleware enforces authentication, API-Version, optional request
// signatures and ACP idempotency in front of an API handler.
type Middleware struct {
	P    Policy
	mu   sync.Mutex
	keys map[string]*idemEntry // identity + path + key
	Hits struct{ Replays, Conflicts, InFlight int }
}

// Wrap returns the handler behind the ACP rules.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	if m.P.Retention < 24*time.Hour {
		m.P.Retention = 24 * time.Hour
	}
	if m.P.MaxSkew == 0 {
		m.P.MaxSkew = 5 * time.Minute
	}
	m.keys = map[string]*idemEntry{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rid := r.Header.Get("Request-Id"); rid != "" {
			w.Header().Set("Request-Id", rid)
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		who, ok := m.P.Tokens[tok]
		if !ok || tok == "" {
			WriteError(w, http.StatusUnauthorized, Error{Type: "invalid_request", Code: "unauthorized", Message: "missing or invalid bearer token"})
			return
		}
		if v := r.Header.Get("API-Version"); !contains(m.P.Versions, v) {
			WriteError(w, http.StatusBadRequest, Error{Type: "invalid_request", Code: "unsupported_api_version", Message: "missing or unsupported API-Version", SupportedVersions: m.P.Versions})
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			WriteError(w, http.StatusBadRequest, Error{Type: "invalid_request", Code: "invalid", Message: "unreadable body"})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if key := m.P.SignerKeys[who]; key != nil {
			if !m.signatureOK(r, body, key) {
				WriteError(w, http.StatusUnauthorized, Error{Type: "invalid_request", Code: "invalid_signature", Message: "Signature or Timestamp header missing, stale or invalid"})
				return
			}
		}
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}

		key := r.Header.Get("Idempotency-Key")
		if key == "" || len(key) > 255 {
			WriteError(w, http.StatusBadRequest, Error{Type: "invalid_request", Code: "idempotency_key_required", Message: "Idempotency-Key header is required on all POST requests"})
			return
		}
		w.Header().Set("Idempotency-Key", key)
		fp, err := Fingerprint(nonEmpty(body))
		if err != nil {
			WriteError(w, http.StatusBadRequest, Error{Type: "invalid_request", Code: "invalid", Message: "body is not valid JSON"})
			return
		}
		scope := who + " " + r.URL.Path + " " + key
		now := m.P.Clock.Now()

		m.mu.Lock()
		if e, ok := m.keys[scope]; ok && now.Sub(e.at) < m.P.Retention {
			switch {
			case e.fingerprint != fp:
				m.Hits.Conflicts++
				m.mu.Unlock()
				WriteError(w, http.StatusUnprocessableEntity, Error{Type: "invalid_request", Code: "idempotency_conflict", Message: "Idempotency-Key has already been used with a different request body"})
				return
			case e.inFlight:
				m.Hits.InFlight++
				m.mu.Unlock()
				w.Header().Set("Retry-After", "1")
				WriteError(w, http.StatusConflict, Error{Type: "invalid_request", Code: "idempotency_in_flight", Message: "A request with this Idempotency-Key is currently being processed"})
				return
			default:
				m.Hits.Replays++
				m.mu.Unlock()
				for k, v := range e.header {
					w.Header()[k] = v
				}
				w.Header().Set("Idempotent-Replayed", "true")
				w.WriteHeader(e.status)
				w.Write(e.body) //nolint:errcheck
				return
			}
		}
		m.keys[scope] = &idemEntry{fingerprint: fp, inFlight: true, at: now}
		m.mu.Unlock()

		rec := &recorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)

		m.mu.Lock()
		if rec.status >= 500 {
			delete(m.keys, scope) // ACP 6.5: never cache 5xx; a retry is fresh
		} else {
			m.keys[scope] = &idemEntry{fingerprint: fp, status: rec.status, body: rec.buf.Bytes(), header: w.Header().Clone(), at: now}
		}
		m.mu.Unlock()
	})
}

func (m *Middleware) signatureOK(r *http.Request, body []byte, key ed25519.PublicKey) bool {
	ts := r.Header.Get("Timestamp")
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return false
	}
	if d := m.P.Clock.Now().Sub(t); d > m.P.MaxSkew || d < -m.P.MaxSkew {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(r.Header.Get("Signature"))
	if err != nil {
		return false
	}
	c, err := Canonical(nonEmpty(body))
	if err != nil {
		return false
	}
	return ed25519.Verify(key, append([]byte(ts+"."), c...), sig)
}

// SignRequest computes the Signature header for a body at a timestamp.
func SignRequest(priv ed25519.PrivateKey, ts string, body []byte) (string, error) {
	c, err := Canonical(nonEmpty(body))
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, append([]byte(ts+"."), c...))), nil
}

type recorder struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (r *recorder) WriteHeader(s int)           { r.status = s; r.ResponseWriter.WriteHeader(s) }
func (r *recorder) Write(b []byte) (int, error) { r.buf.Write(b); return r.ResponseWriter.Write(b) }

func nonEmpty(b []byte) []byte {
	if len(bytes.TrimSpace(b)) == 0 {
		return []byte("{}")
	}
	return b
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
