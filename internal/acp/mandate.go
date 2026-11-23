package acp

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Mandate is the user's signed approval for an agent to pay: who pays, which
// merchant, how much at most, for which checkout, until when. It is signed
// on the user's device (the wallet) with a key the agent never sees, in the
// role AP2 gives its payment mandate. The PSP creates a Reserve Pay block
// only for a mandate whose signature verifies, and every capture is checked
// against it again.
type Mandate struct {
	PayerVPA          string `json:"payer_vpa"`
	MerchantID        string `json:"merchant_id"`
	MaxAmount         int64  `json:"max_amount"` // paise
	Currency          string `json:"currency"`   // "inr"
	CheckoutSessionID string `json:"checkout_session_id"`
	ExpiresAt         string `json:"expires_at"` // RFC 3339
	Nonce             string `json:"nonce"`
}

// SignedMandate carries the signature (base64url, Ed25519 over the
// canonical JSON of Mandate) and the signer's key ID.
type SignedMandate struct {
	Mandate   Mandate `json:"mandate"`
	KeyID     string  `json:"key_id"`
	Signature string  `json:"signature"`
}

// Mandate errors.
var (
	ErrMandateSignature = errors.New("acp: mandate signature does not verify")
	ErrMandateKey       = errors.New("acp: unknown mandate key")
	ErrMandateReplay    = errors.New("acp: mandate nonce already used")
	ErrMandateExpired   = errors.New("acp: mandate expired")
	ErrMandateMismatch  = errors.New("acp: mandate does not match the request")
)

// SignMandate signs m with the user's private key.
func SignMandate(m Mandate, keyID string, priv ed25519.PrivateKey) (SignedMandate, error) {
	msg, err := CanonicalValue(m)
	if err != nil {
		return SignedMandate{}, err
	}
	return SignedMandate{Mandate: m, KeyID: keyID, Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, msg))}, nil
}

// Keyring holds users' registered public keys (registered with the PSP at
// onboarding, after device binding) and the nonces already used.
type Keyring struct {
	mu     sync.Mutex
	keys   map[string]keyEntry // key ID -> key and owner VPA
	nonces map[string]bool
}

type keyEntry struct {
	pub ed25519.PublicKey
	vpa string
}

// NewKeyring returns an empty keyring.
func NewKeyring() *Keyring {
	return &Keyring{keys: map[string]keyEntry{}, nonces: map[string]bool{}}
}

// Register binds a public key to a UPI ID.
func (k *Keyring) Register(keyID, vpa string, pub ed25519.PublicKey) {
	k.mu.Lock()
	k.keys[keyID] = keyEntry{pub, vpa}
	k.mu.Unlock()
}

// Verify checks the signature, that the key belongs to the payer, that the
// mandate has not expired, and consumes its nonce (single use).
func (k *Keyring) Verify(sm SignedMandate, now time.Time) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	e, ok := k.keys[sm.KeyID]
	if !ok {
		return ErrMandateKey
	}
	if e.vpa != sm.Mandate.PayerVPA {
		return fmt.Errorf("%w: key belongs to %s", ErrMandateKey, e.vpa)
	}
	sig, err := base64.RawURLEncoding.DecodeString(sm.Signature)
	if err != nil {
		return ErrMandateSignature
	}
	msg, err := CanonicalValue(sm.Mandate)
	if err != nil || !ed25519.Verify(e.pub, msg, sig) {
		return ErrMandateSignature
	}
	exp, err := time.Parse(time.RFC3339, sm.Mandate.ExpiresAt)
	if err != nil || !now.Before(exp) {
		return ErrMandateExpired
	}
	if sm.Mandate.Nonce == "" || k.nonces[sm.Mandate.Nonce] {
		return ErrMandateReplay
	}
	k.nonces[sm.Mandate.Nonce] = true
	return nil
}
