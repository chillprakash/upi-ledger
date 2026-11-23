package acp_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chillprakash/upi-ledger/internal/acp"
	"github.com/chillprakash/upi-ledger/internal/bank"
	"github.com/chillprakash/upi-ledger/internal/ledger"
	"github.com/chillprakash/upi-ledger/internal/money"
	"github.com/chillprakash/upi-ledger/internal/netsim"
	"github.com/chillprakash/upi-ledger/internal/npci"
	"github.com/chillprakash/upi-ledger/internal/upitest"
)

var ctx = context.Background()

const version = "2026-04-17"

// env is asha@hdfc (wallet key registered) buying from "ravistores"
// (shop@icici) through an agent platform holding bearer token "agent-token".
type env struct {
	t        *testing.T
	w        *upitest.World
	psp      *acp.PSP
	merchant *acp.Merchant
	keys     *acp.Keyring
	priv     ed25519.PrivateKey
	pspSrv   *httptest.Server
	merchSrv *httptest.Server
	nonce    int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	w := upitest.New(t)
	w.Switch.Register(npci.Address{VPA: "shop@icici", Bank: "icici", Account: "ravi", Name: "RAVI STORES"})
	keys := acp.NewKeyring()
	pub, priv, _ := ed25519.GenerateKey(nil)
	keys.Register("asha-phone", "asha@hdfc", pub)
	psp := acp.NewPSP(w.Switch, map[string]*bank.Bank{"hdfc": w.HDFC, "icici": w.ICICI}, w.Clock, keys, map[string]string{"ravistores": "shop@icici"})
	n := 0
	psp.IDs = func() string { n++; return fmt.Sprintf("vt_upi_%04d", n) }
	merchant := acp.NewMerchant("ravistores", "shop@icici", []acp.Product{
		{ID: "item_123", Name: "Tea 500 g", UnitAmount: 25000},
		{ID: "rice", Name: "Rice 5 kg", UnitAmount: 60000},
		{ID: "oil", Name: "Oil 1 l", UnitAmount: 18000},
	}, psp, w.Clock)
	pol := func() acp.Policy {
		return acp.Policy{Versions: []string{"2026-01-16", "2026-01-30", version}, Tokens: map[string]string{"agent-token": "agent-platform"}, Clock: w.Clock}
	}
	e := &env{t: t, w: w, psp: psp, merchant: merchant, keys: keys, priv: priv}
	e.pspSrv = httptest.NewServer((&acp.Middleware{P: pol()}).Wrap(psp.Handler()))
	e.merchSrv = httptest.NewServer((&acp.Middleware{P: pol()}).Wrap(merchant.Handler()))
	t.Cleanup(func() { e.pspSrv.Close(); e.merchSrv.Close() })
	return e
}

type resp struct {
	status int
	header http.Header
	body   map[string]any
	raw    []byte
}

func (e *env) do(srv *httptest.Server, method, path, key string, body any, mod ...func(*http.Request)) resp {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		var b []byte
		switch x := body.(type) {
		case string:
			b = []byte(x)
		default:
			b, _ = json.Marshal(x)
		}
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rd)
	req.Header.Set("Authorization", "Bearer agent-token")
	req.Header.Set("API-Version", version)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	for _, f := range mod {
		f(req)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	r := resp{status: res.StatusCode, header: res.Header, raw: raw, body: map[string]any{}}
	json.Unmarshal(raw, &r.body) //nolint:errcheck
	return r
}

// mandate signs an approval on the user's wallet.
func (e *env) mandate(session string, max int64, expires time.Time) acp.SignedMandate {
	e.nonce++
	sm, err := acp.SignMandate(acp.Mandate{PayerVPA: "asha@hdfc", MerchantID: "ravistores", MaxAmount: max, Currency: "inr",
		CheckoutSessionID: session, ExpiresAt: expires.UTC().Format(time.RFC3339), Nonce: fmt.Sprintf("n%d", e.nonce)}, "asha-phone", e.priv)
	if err != nil {
		e.t.Fatal(err)
	}
	return sm
}

func (e *env) delegateReq(session string, max int64, sm *acp.SignedMandate) acp.DelegateRequest {
	var r acp.DelegateRequest
	r.PaymentMethod.Type, r.PaymentMethod.VPA = acp.PaymentMethodType, "asha@hdfc"
	exp := e.w.Clock.Now().Add(time.Hour)
	if sm != nil {
		exp, _ = time.Parse(time.RFC3339, sm.Mandate.ExpiresAt)
	}
	r.Allowance = acp.Allowance{Reason: "one_time", MaxAmount: max, Currency: "inr", CheckoutSessionID: session, MerchantID: "ravistores", ExpiresAt: exp.UTC().Format(time.RFC3339)}
	r.Metadata = map[string]string{"source": "test"}
	r.UserMandate = sm
	return r
}

// delegate gets a vault token for a session.
func (e *env) delegate(session string, max int64) string {
	e.t.Helper()
	sm := e.mandate(session, max, e.w.Clock.Now().Add(time.Hour))
	r := e.do(e.pspSrv, "POST", "/agentic_commerce/delegate_payment", "d-"+session+fmt.Sprint(e.nonce), e.delegateReq(session, max, &sm))
	if r.status != 201 {
		e.t.Fatalf("delegate: %d %s", r.status, r.raw)
	}
	return r.body["id"].(string)
}

func (e *env) create(items string) string {
	e.t.Helper()
	r := e.do(e.merchSrv, "POST", "/checkout_sessions", fmt.Sprintf("c-%d", time.Now().UnixNano()), `{"line_items":`+items+`}`)
	if r.status != 201 {
		e.t.Fatalf("create: %d %s", r.status, r.raw)
	}
	return r.body["id"].(string)
}

func complete(token string) map[string]any {
	return map[string]any{"payment_data": map[string]any{"handler_id": acp.HandlerID, "instrument": map[string]any{"type": acp.PaymentMethodType, "credential": map[string]any{"type": "vault_token", "token": token}}}}
}

func (e *env) avail(acct string) money.Paise { return e.w.Balance(e.t, e.w.HDFC, "asha") }

// --- HTTP rules (ACP 3.1, 6) -------------------------------------------------

func TestAuthAndVersion(t *testing.T) {
	e := newEnv(t)
	if r := e.do(e.merchSrv, "GET", "/checkout_sessions/x", "", nil, func(q *http.Request) { q.Header.Del("Authorization") }); r.status != 401 {
		t.Fatalf("no bearer: %d", r.status)
	}
	r := e.do(e.merchSrv, "GET", "/checkout_sessions/x", "", nil, func(q *http.Request) { q.Header.Set("API-Version", "2024-01-01") })
	if r.status != 400 || r.body["code"] != "unsupported_api_version" || r.body["supported_versions"] == nil {
		t.Fatalf("bad version: %d %s", r.status, r.raw)
	}
	if r := e.do(e.merchSrv, "GET", "/checkout_sessions/x", "", nil, func(q *http.Request) { q.Header.Set("Request-Id", "rq1") }); r.header.Get("Request-Id") != "rq1" || r.status != 404 {
		t.Fatalf("request id echo / 404: %d %q", r.status, r.header.Get("Request-Id"))
	}
}

func TestIdempotency(t *testing.T) {
	e := newEnv(t)
	if r := e.do(e.merchSrv, "POST", "/checkout_sessions", "", `{"line_items":[{"id":"item_123"}]}`); r.status != 400 || r.body["code"] != "idempotency_key_required" {
		t.Fatalf("missing key: %d %s", r.status, r.raw)
	}
	a := e.do(e.merchSrv, "POST", "/checkout_sessions", "k1", `{"line_items":[{"id":"item_123","quantity":1}]}`)
	// Same body with different key order and 1.0 for 1: a replay (ACP 6.2).
	b := e.do(e.merchSrv, "POST", "/checkout_sessions", "k1", `{"line_items":[{"quantity":1.0,"id":"item_123"}]}`)
	if a.status != 201 || b.status != 201 || b.header.Get("Idempotent-Replayed") != "true" || a.body["id"] != b.body["id"] || b.header.Get("Idempotency-Key") != "k1" {
		t.Fatalf("replay: %d %d %v %v %q", a.status, b.status, a.body["id"], b.body["id"], b.header.Get("Idempotent-Replayed"))
	}
	if len(e.merchant.Sessions()) != 1 {
		t.Fatal("replay re-executed the create")
	}
	c := e.do(e.merchSrv, "POST", "/checkout_sessions", "k1", `{"line_items":[{"id":"rice"}]}`)
	if c.status != 422 || c.body["code"] != "idempotency_conflict" {
		t.Fatalf("conflict: %d %s", c.status, c.raw)
	}
	// null differs from absent (ACP 6.2).
	if d := e.do(e.merchSrv, "POST", "/checkout_sessions", "k1", `{"line_items":[{"id":"item_123","quantity":1}],"buyer":null}`); d.status != 422 {
		t.Fatalf("null vs absent treated equal: %d", d.status)
	}
	// Keys are scoped per endpoint path (ACP 6.1).
	id := a.body["id"].(string)
	if u := e.do(e.merchSrv, "POST", "/checkout_sessions/"+id, "k1", `{"line_items":[{"id":"rice"}]}`); u.status != 200 {
		t.Fatalf("same key on another path: %d %s", u.status, u.raw)
	}
	// Retained 24 h, then a reused key is a new request (ACP 6.6).
	e.w.Clock.Advance(24*time.Hour + time.Second)
	if f := e.do(e.merchSrv, "POST", "/checkout_sessions", "k1", `{"line_items":[{"id":"rice"}]}`); f.status != 201 || f.header.Get("Idempotent-Replayed") == "true" {
		t.Fatalf("after retention: %d", f.status)
	}
}

func TestInFlightAndServerErrors(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	var calls atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			<-release
		}
		if r.URL.Path == "/fail" {
			acp.WriteError(w, 503, acp.Error{Type: "service_unavailable", Code: "busy", Message: "try later"})
			return
		}
		acp.WriteJSON(w, 200, map[string]int32{"call": calls.Load()})
	})
	srv := httptest.NewServer((&acp.Middleware{P: acp.Policy{Versions: []string{version}, Tokens: map[string]string{"agent-token": "a"}, Clock: e.w.Clock}}).Wrap(h))
	defer srv.Close()
	done := make(chan resp)
	go func() { done <- e.do(srv, "POST", "/slow", "k", `{}`) }()
	time.Sleep(50 * time.Millisecond)
	if r := e.do(srv, "POST", "/slow", "k", `{}`); r.status != 409 || r.body["code"] != "idempotency_in_flight" || r.header.Get("Retry-After") == "" {
		t.Fatalf("in flight: %d %s", r.status, r.raw)
	}
	close(release)
	<-done
	// 5xx is never cached: the retry runs again (ACP 6.5).
	a := e.do(srv, "POST", "/fail", "k5", `{}`)
	b := e.do(srv, "POST", "/fail", "k5", `{}`)
	if a.status != 503 || b.status != 503 || b.header.Get("Idempotent-Replayed") == "true" || calls.Load() != 3 {
		t.Fatalf("5xx cached: %d %d calls=%d", a.status, b.status, calls.Load())
	}
}

func TestRequestSignatures(t *testing.T) {
	e := newEnv(t)
	pub, priv, _ := ed25519.GenerateKey(nil)
	pol := acp.Policy{Versions: []string{version}, Tokens: map[string]string{"agent-token": "platform"}, SignerKeys: map[string]ed25519.PublicKey{"platform": pub}, Clock: e.w.Clock}
	srv := httptest.NewServer((&acp.Middleware{P: pol}).Wrap(e.merchant.Handler()))
	defer srv.Close()
	body := `{"line_items":[{"id":"item_123"}]}`
	sign := func(ts string, b string) func(*http.Request) {
		return func(q *http.Request) {
			sig, _ := acp.SignRequest(priv, ts, []byte(b))
			q.Header.Set("Timestamp", ts)
			q.Header.Set("Signature", sig)
		}
	}
	now := e.w.Clock.Now().UTC().Format(time.RFC3339)
	if r := e.do(srv, "POST", "/checkout_sessions", "s1", body, sign(now, body)); r.status != 201 {
		t.Fatalf("valid signature: %d %s", r.status, r.raw)
	}
	if r := e.do(srv, "POST", "/checkout_sessions", "s2", body, sign(now, `{"line_items":[{"id":"rice"}]}`)); r.status != 401 {
		t.Fatalf("signature over another body: %d", r.status)
	}
	old := e.w.Clock.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	if r := e.do(srv, "POST", "/checkout_sessions", "s3", body, sign(old, body)); r.status != 401 {
		t.Fatalf("stale timestamp: %d", r.status)
	}
	if r := e.do(srv, "POST", "/checkout_sessions", "s4", body); r.status != 401 {
		t.Fatalf("unsigned: %d", r.status)
	}
}

func TestCanonical(t *testing.T) {
	eq := func(a, b string) bool {
		x, _ := acp.Fingerprint([]byte(a))
		y, _ := acp.Fingerprint([]byte(b))
		return x == y
	}
	if !eq(`{"a":1,"b":[1,2]}`, `{"b":[1.0,2],"a":1}`) || eq(`{"a":[1,2]}`, `{"a":[2,1]}`) || eq(`{"a":null}`, `{}`) {
		t.Fatal("canonical equivalence rules")
	}
}

// --- Delegate payment ---------------------------------------------------------

func TestDelegateCreatesBlock(t *testing.T) {
	e := newEnv(t)
	s := e.create(`[{"id":"item_123"}]`)
	tok := e.delegate(s, 30000)
	if !strings.HasPrefix(tok, "vt_upi_") {
		t.Fatal(tok)
	}
	if got := e.avail("asha"); got != money.Rupees(700) {
		t.Fatalf("Rs 300 not reserved: available %s", got)
	}
	if b, ok := e.w.HDFC.GetBlock("acp-" + tok); !ok || b.Status != "active" || b.Merchant != "shop@icici" {
		t.Fatalf("block %+v", b)
	}
}

func TestDelegateRejects(t *testing.T) {
	e := newEnv(t)
	s := e.create(`[{"id":"item_123"}]`)
	exp := e.w.Clock.Now().Add(time.Hour)
	cases := map[string]struct {
		edit   func(*acp.DelegateRequest)
		status int
		code   string
	}{
		"card is not this handler": {func(r *acp.DelegateRequest) { r.PaymentMethod.Type = "card" }, 400, "invalid"},
		"reason":                   {func(r *acp.DelegateRequest) { r.Allowance.Reason = "recurring" }, 400, "invalid"},
		"currency":                 {func(r *acp.DelegateRequest) { r.Allowance.Currency = "usd" }, 400, "invalid"},
		"unknown merchant":         {func(r *acp.DelegateRequest) { r.Allowance.MerchantID = "evil" }, 400, "invalid"},
		"no mandate":               {func(r *acp.DelegateRequest) { r.UserMandate = nil }, 401, "mandate_required"},
		"amount above mandate":     {func(r *acp.DelegateRequest) { r.Allowance.MaxAmount = 99999 }, 422, "mandate_mismatch"},
		"other session":            {func(r *acp.DelegateRequest) { r.Allowance.CheckoutSessionID = "cs_other" }, 422, "mandate_mismatch"},
		"tampered after signing": {func(r *acp.DelegateRequest) {
			r.UserMandate.Mandate.MaxAmount = 99999
			r.Allowance.MaxAmount = 99999
		}, 401, "mandate_invalid"},
		"over 48 h": {func(r *acp.DelegateRequest) {
			r.Allowance.ExpiresAt = exp.Add(72 * time.Hour).UTC().Format(time.RFC3339)
		}, 400, "invalid"},
		"no metadata": {func(r *acp.DelegateRequest) { r.Metadata = nil }, 400, "missing"},
	}
	i := 0
	for name, c := range cases {
		sm := e.mandate(s, 30000, exp)
		req := e.delegateReq(s, 30000, &sm)
		c.edit(&req)
		i++
		r := e.do(e.pspSrv, "POST", "/agentic_commerce/delegate_payment", fmt.Sprintf("bad-%d", i), req)
		if r.status != c.status || r.body["code"] != c.code {
			t.Errorf("%s: %d %v (want %d %s)", name, r.status, r.body["code"], c.status, c.code)
		}
	}
	if got := e.avail("asha"); got != money.Rupees(1000) {
		t.Fatalf("a rejected request reserved money: %s", got)
	}
}

func TestMandateKeyReplayAndExpiry(t *testing.T) {
	e := newEnv(t)
	s := e.create(`[{"id":"item_123"}]`)
	sm := e.mandate(s, 30000, e.w.Clock.Now().Add(time.Hour))
	if r := e.do(e.pspSrv, "POST", "/agentic_commerce/delegate_payment", "m1", e.delegateReq(s, 30000, &sm)); r.status != 201 {
		t.Fatal(r.status)
	}
	// Same signed mandate, new idempotency key: the nonce is spent.
	if r := e.do(e.pspSrv, "POST", "/agentic_commerce/delegate_payment", "m2", e.delegateReq(s, 30000, &sm)); r.status != 401 {
		t.Fatalf("mandate replay: %d %s", r.status, r.raw)
	}
	// A key registered to someone else cannot sign for asha.
	pub2, priv2, _ := ed25519.GenerateKey(nil)
	e.keys.Register("ravi-phone", "ravi@icici", pub2)
	forged, _ := acp.SignMandate(acp.Mandate{PayerVPA: "asha@hdfc", MerchantID: "ravistores", MaxAmount: 30000, Currency: "inr", CheckoutSessionID: s, ExpiresAt: sm.Mandate.ExpiresAt, Nonce: "x1"}, "ravi-phone", priv2)
	if r := e.do(e.pspSrv, "POST", "/agentic_commerce/delegate_payment", "m3", e.delegateReq(s, 30000, &forged)); r.status != 401 {
		t.Fatalf("foreign key: %d", r.status)
	}
	if err := e.keys.Verify(e.mandate(s, 1, e.w.Clock.Now().Add(-time.Minute)), e.w.Clock.Now()); !errors.Is(err, acp.ErrMandateExpired) {
		t.Fatalf("expired mandate: %v", err)
	}
}

func TestDelegateInsufficientFunds(t *testing.T) {
	e := newEnv(t)
	s := e.create(`[{"id":"item_123"}]`)
	sm := e.mandate(s, 200000, e.w.Clock.Now().Add(time.Hour))
	if r := e.do(e.pspSrv, "POST", "/agentic_commerce/delegate_payment", "f1", e.delegateReq(s, 200000, &sm)); r.status != 422 || r.body["code"] != "insufficient_funds" {
		t.Fatalf("got %d %s", r.status, r.raw)
	}
}

// --- Capture rules ---------------------------------------------------------------

func TestCaptureRules(t *testing.T) {
	e := newEnv(t)
	s := e.create(`[{"id":"item_123"}]`)
	tok := e.delegate(s, 30000)
	for name, c := range map[string]struct {
		merchant, session string
		amt               money.Paise
		code              string
	}{
		"wrong merchant": {"evil", s, 26250, acp.CodeWrongMerch},
		"wrong session":  {"ravistores", "cs_x", 26250, acp.CodeWrongSession},
		"zero":           {"ravistores", s, 0, acp.CodeBadAmount},
		"unknown token":  {"ravistores", s, 100, acp.CodeUnknownToken},
	} {
		id := tok
		if name == "unknown token" {
			id = "vt_upi_nope"
		}
		if r := e.psp.Capture(ctx, id, c.merchant, c.session, c.amt); r.Status != acp.CaptureDeclined || r.Code != c.code {
			t.Errorf("%s: %+v", name, r)
		}
	}
	if r := e.psp.Capture(ctx, tok, "ravistores", s, 26250); r.Status != acp.CaptureSuccess {
		t.Fatalf("capture: %+v", r)
	}
	// Repeat of the same capture: status, no second debit.
	if r := e.psp.Capture(ctx, tok, "ravistores", s, 26250); r.Status != acp.CaptureSuccess {
		t.Fatalf("repeat: %+v", r)
	}
	// Any other use of a one-time token is refused.
	if r := e.psp.Capture(ctx, tok, "ravistores", s, 100); r.Code != acp.CodeUsed {
		t.Fatalf("reuse: %+v", r)
	}
	// Rs 262.50 paid; the rest of the Rs 300 block is released.
	if got := e.avail("asha"); got != money.Rupees(1000)-26250 {
		t.Fatalf("asha available %s", got)
	}
	e.w.CheckLedgers(t)
}

// Over the allowance: refused, and the reservation is freed at once.
func TestOverAllowanceReleasesReservation(t *testing.T) {
	e := newEnv(t)
	s := e.create(`[{"id":"item_123"}]`)
	tok := e.delegate(s, 30000)
	if r := e.psp.Capture(ctx, tok, "ravistores", s, 30001); r.Code != acp.CodeOverLimit {
		t.Fatalf("got %+v", r)
	}
	if got := e.avail("asha"); got != money.Rupees(1000) {
		t.Fatalf("reservation kept after an impossible capture: %s", got)
	}
	if r := e.psp.Capture(ctx, tok, "ravistores", s, 100); r.Status != acp.CaptureDeclined {
		t.Fatalf("token still usable: %+v", r)
	}
}

func TestExpiryAndRevoke(t *testing.T) {
	e := newEnv(t)
	s := e.create(`[{"id":"item_123"}]`)
	tok := e.delegate(s, 30000)
	if err := e.psp.Revoke(ctx, tok, "ravi@icici"); err == nil {
		t.Fatal("someone else revoked asha's allowance")
	}
	if err := e.psp.Revoke(ctx, tok, "asha@hdfc"); err != nil {
		t.Fatal(err)
	}
	if r := e.psp.Capture(ctx, tok, "ravistores", s, 100); r.Code != acp.CodeRevoked {
		t.Fatalf("after revoke: %+v", r)
	}
	tok2 := e.delegate(s, 30000)
	e.w.Clock.Advance(61 * time.Minute)
	if r := e.psp.Capture(ctx, tok2, "ravistores", s, 100); r.Code != acp.CodeExpired {
		t.Fatalf("after expiry: %+v", r)
	}
	e.psp.Sweep(ctx)
	if got := e.avail("asha"); got != money.Rupees(1000) {
		t.Fatalf("reservations not released: %s", got)
	}
}

func TestNewMandateSupersedesUnusedAllowance(t *testing.T) {
	e := newEnv(t)
	s := e.create(`[{"id":"item_123"}]`)
	old := e.delegate(s, 30000)
	tok := e.delegate(s, 40000)
	if r := e.psp.Capture(ctx, old, "ravistores", s, 100); r.Code != acp.CodeRevoked {
		t.Fatalf("superseded token still usable: %+v", r)
	}
	if got := e.avail("asha"); got != money.Rupees(600) {
		t.Fatalf("only the new Rs 400 should be reserved: %s", got)
	}
	_ = tok
}

func TestConcurrentCapturesDebitOnce(t *testing.T) {
	e := newEnv(t)
	s := e.create(`[{"id":"item_123"}]`)
	tok := e.delegate(s, 30000)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.psp.Capture(ctx, tok, "ravistores", s, 26250)
		}()
	}
	wg.Wait()
	if got := e.avail("asha"); got != money.Rupees(1000)-26250 {
		t.Fatalf("asha %s after 20 concurrent captures", got)
	}
	e.w.CheckLedgers(t)
}

// --- Checkout --------------------------------------------------------------------

func TestCheckoutFlow(t *testing.T) {
	e := newEnv(t)
	s := e.create(`[{"id":"item_123","quantity":2}]`)
	got, _ := e.merchant.Get(s)
	if got.Status != "ready_for_payment" || got.Currency != "inr" || len(got.Capabilities.Payment.Handlers) != 1 {
		t.Fatalf("%+v", got)
	}
	// Rs 500 + 5% GST = Rs 525.
	if u := e.do(e.merchSrv, "POST", "/checkout_sessions/"+s, "u1", `{"line_items":[{"id":"item_123"}]}`); u.status != 200 {
		t.Fatal(u.status)
	}
	tok := e.delegate(s, 26250)
	r := e.do(e.merchSrv, "POST", "/checkout_sessions/"+s+"/complete", "cmp1", complete(tok))
	if r.status != 200 || r.body["status"] != "completed" || r.body["order"] == nil {
		t.Fatalf("complete: %d %s", r.status, r.raw)
	}
	// A second complete with another key: same state, no second debit.
	if r2 := e.do(e.merchSrv, "POST", "/checkout_sessions/"+s+"/complete", "cmp2", complete(tok)); r2.body["status"] != "completed" {
		t.Fatalf("second complete: %s", r2.raw)
	}
	if got := e.avail("asha"); got != money.Rupees(1000)-26250 {
		t.Fatalf("asha %s", got)
	}
	if c := e.do(e.merchSrv, "POST", "/checkout_sessions/"+s+"/cancel", "x1", `{}`); c.status != 405 {
		t.Fatalf("cancel after complete: %d", c.status)
	}
	if ev := e.merchant.Events(); len(ev) != 1 || ev[0].Type != "order_create" {
		t.Fatalf("events %+v", ev)
	}
}

func TestCompleteWithTooSmallAllowanceIsDeclined(t *testing.T) {
	e := newEnv(t)
	s := e.create(`[{"id":"rice"}]`) // Rs 630
	tok := e.delegate(s, 50000)
	r := e.do(e.merchSrv, "POST", "/checkout_sessions/"+s+"/complete", "c1", complete(tok))
	msgs, _ := r.body["messages"].([]any)
	if r.body["status"] != "ready_for_payment" || len(msgs) == 0 || msgs[0].(map[string]any)["code"] != "payment_declined" {
		t.Fatalf("got %s", r.raw)
	}
	if got := e.avail("asha"); got != money.Rupees(1000) {
		t.Fatalf("money moved on a declined capture: %s", got)
	}
}

func TestUncertainOutcomeSettlesLater(t *testing.T) {
	e := newEnv(t)
	s := e.create(`[{"id":"item_123"}]`)
	tok := e.delegate(s, 26250)
	e.w.Fault("npci>icici", netsim.LostResponse) // credit applied, reply lost: deemed
	r := e.do(e.merchSrv, "POST", "/checkout_sessions/"+s+"/complete", "c1", complete(tok))
	if r.body["status"] != "in_progress" {
		t.Fatalf("got %s", r.raw)
	}
	e.w.Switch.Resolve(ctx)
	e.merchant.Reconcile(ctx)
	if got, _ := e.merchant.Get(s); got.Status != "completed" || got.Order == nil {
		t.Fatalf("after reconcile: %+v", got)
	}
}

// --- Conformance with ACP's published examples (2026-04-17) ----------------------

func TestConformanceWithSpecExamples(t *testing.T) {
	raw, err := os.ReadFile("testdata/acp-2026-04-17-examples.agentic_checkout.json")
	if err != nil {
		t.Fatal(err)
	}
	var ex map[string]json.RawMessage
	if err := json.Unmarshal(raw, &ex); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	// The spec's create request (its item_123 is in our catalog).
	r := e.do(e.merchSrv, "POST", "/checkout_sessions", "conf1", string(ex["create_checkout_session_request"]))
	if r.status != 201 {
		t.Fatalf("spec create request rejected: %d %s", r.status, r.raw)
	}
	for _, f := range []string{"id", "capabilities", "status", "currency", "line_items", "totals", "messages", "links"} {
		if _, ok := r.body[f]; !ok {
			t.Errorf("create response missing %s", f)
		}
	}
	h := r.body["capabilities"].(map[string]any)["payment"].(map[string]any)["handlers"].([]any)[0].(map[string]any)
	for _, f := range []string{"id", "name", "version", "spec", "requires_delegate_payment", "requires_pci_compliance", "psp", "config"} {
		if _, ok := h[f]; !ok {
			t.Errorf("payment handler missing %s", f)
		}
	}
	for _, li := range r.body["line_items"].([]any) {
		for _, f := range []string{"base_amount", "discount", "subtotal", "tax", "total"} {
			if v, ok := li.(map[string]any)[f].(float64); !ok || v != float64(int64(v)) {
				t.Errorf("line item %s not an integer", f)
			}
		}
	}
	// Error bodies match the spec's examples (type and code; the RFC text
	// is normative for the message).
	check := func(name string, got resp) {
		var want map[string]any
		json.Unmarshal(ex[name], &want) //nolint:errcheck
		if got.body["type"] != want["type"] || got.body["code"] != want["code"] {
			t.Errorf("%s: got %s, spec %s", name, got.raw, ex[name])
		}
		if p, ok := want["param"]; ok && got.body["param"] != p {
			t.Errorf("%s param: got %v, spec %v", name, got.body["param"], p)
		}
	}
	check("error_400_idempotency_key_required", e.do(e.merchSrv, "POST", "/checkout_sessions", "", `{"line_items":[{"id":"item_123"}]}`))
	e.do(e.merchSrv, "POST", "/checkout_sessions", "conf2", `{"line_items":[{"id":"item_123"}]}`)
	check("error_422_idempotency_conflict", e.do(e.merchSrv, "POST", "/checkout_sessions", "conf2", `{"line_items":[{"id":"rice"}]}`))
	check("error_400_invalid_item", e.do(e.merchSrv, "POST", "/checkout_sessions", "conf3", `{"line_items":[{"id":"invalid_item"}]}`))
	// The spec's card complete request is refused cleanly by a UPI-only merchant.
	s := e.create(`[{"id":"item_123"}]`)
	if c := e.do(e.merchSrv, "POST", "/checkout_sessions/"+s+"/complete", "conf4", string(ex["complete_checkout_session_request"])); c.status != 400 || c.body["type"] != "invalid_request" {
		t.Errorf("card complete: %d %s", c.status, c.raw)
	}
}

// Two people each approve the same cart (say, two family members) and the
// agent fires both completes at once: the order must be paid exactly once.
func TestConcurrentCompletesWithTwoTokensPayOnce(t *testing.T) {
	e := newEnv(t)
	payer := func(name string) ed25519.PrivateKey {
		if err := e.w.HDFC.OpenAccount(ctx, ledgerID(name), money.Rupees(100000)); err != nil {
			t.Fatal(err)
		}
		e.w.Switch.Register(npci.Address{VPA: name + "@hdfc", Bank: "hdfc", Account: ledgerID(name), Name: name})
		pub, priv, _ := ed25519.GenerateKey(nil)
		e.keys.Register(name+"-phone", name+"@hdfc", pub)
		return priv
	}
	kpriv, mpriv := payer("kavi"), payer("meena")
	sign := func(name string, priv ed25519.PrivateKey, s string, round int) acp.SignedMandate {
		sm, _ := acp.SignMandate(acp.Mandate{PayerVPA: name + "@hdfc", MerchantID: "ravistores", MaxAmount: 20000, Currency: "inr", CheckoutSessionID: s,
			ExpiresAt: e.w.Clock.Now().Add(time.Hour).UTC().Format(time.RFC3339), Nonce: fmt.Sprintf("%s-%d", name, round)}, name+"-phone", priv)
		return sm
	}
	for round := 0; round < 30; round++ {
		s := e.create(`[{"id":"oil"}]`)
		if s == "" {
			t.Fatal("no session")
		}
		var toks []string
		for _, who := range []struct {
			name string
			priv ed25519.PrivateKey
		}{{"kavi", kpriv}, {"meena", mpriv}} {
			sm := sign(who.name, who.priv, s, round)
			req := e.delegateReq(s, 20000, &sm)
			req.PaymentMethod.VPA = who.name + "@hdfc"
			tk, _, aerr := e.psp.Delegate(ctx, req)
			if aerr != nil {
				t.Fatalf("%s delegate: %+v", who.name, aerr)
			}
			toks = append(toks, tk.ID)
		}
		tokA, tbID := toks[0], toks[1]
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, tok := range []string{tokA, tbID} {
			wg.Add(1)
			go func(tok string) {
				defer wg.Done()
				<-start
				var pd acp.PaymentData
				pd.HandlerID, pd.Instrument.Type = acp.HandlerID, acp.PaymentMethodType
				pd.Instrument.Credential.Type, pd.Instrument.Credential.Token = "vault_token", tok
				e.merchant.Complete(ctx, s, pd) //nolint:errcheck
			}(tok)
		}
		close(start)
		wg.Wait()
		paid := 0
		for _, r := range e.w.Switch.Records() {
			if r.Status == npci.Success && (r.TxnID == "ACP"+tokA || r.TxnID == "ACP"+tbID) {
				paid++
			}
		}
		if paid != 1 {
			t.Fatalf("round %d: order %s paid %d times", round, s, paid)
		}
	}
	e.w.CheckLedgers(t)
}

func ledgerID(name string) ledger.AccountID { return ledger.AccountID(name) }
