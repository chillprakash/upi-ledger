// Package agent is a shopping agent's world: the tools it may call (search,
// checkout, ask the user to approve a payment, complete), the user's wallet
// that alone can sign approvals, and brains that decide which tool to call
// next. Brains can be Claude (see brain_claude.go, brain_cli.go) or scripted
// policies, honest and adversarial, used by the simulator to prove the rails
// hold whatever the agent does.
package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/chillprakash/upi-ledger/internal/acp"
	"github.com/chillprakash/upi-ledger/internal/clock"
	"github.com/chillprakash/upi-ledger/internal/money"
)

// Wallet is the user's device: it holds the signing key and the user's own
// spending rule. The agent can ask; only the wallet can sign.
type Wallet struct {
	VPA    string
	KeyID  string
	Priv   ed25519.PrivateKey
	Budget money.Paise     // most the user approves for one checkout
	Trust  map[string]bool // merchants the user will pay
	Clock  clock.Clock
	TTL    time.Duration
	mu     sync.Mutex
	nonce  int
	Asked  int
	Signed int
}

// Approve signs a mandate if the request is within the user's rule.
func (w *Wallet) Approve(merchantID, session string, max money.Paise) (acp.SignedMandate, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.Asked++
	switch {
	case !w.Trust[merchantID]:
		return acp.SignedMandate{}, fmt.Errorf("user declined: does not pay %s", merchantID)
	case max <= 0 || max > w.Budget:
		return acp.SignedMandate{}, fmt.Errorf("user declined: Rs %s is above the Rs %s budget", max, w.Budget)
	}
	w.nonce++
	ttl := w.TTL
	if ttl == 0 {
		ttl = time.Hour
	}
	m := acp.Mandate{PayerVPA: w.VPA, MerchantID: merchantID, MaxAmount: int64(max), Currency: "inr", CheckoutSessionID: session,
		ExpiresAt: w.Clock.Now().Add(ttl).UTC().Format(time.RFC3339), Nonce: fmt.Sprintf("%s-%d", w.KeyID, w.nonce)}
	sm, err := acp.SignMandate(m, w.KeyID, w.Priv)
	if err == nil {
		w.Signed++
	}
	return sm, err
}

// Shop is the merchant side the agent talks to (in-process or over HTTP).
type Shop interface {
	MerchantID() string
	Search(query string) []acp.Product
	Create(items []acp.Item) (acp.Session, error)
	Update(id string, items []acp.Item) (acp.Session, error)
	Get(id string) (acp.Session, error)
	Complete(id, token string) (acp.Session, error)
	Cancel(id string) (acp.Session, error)
}

// Delegator turns a signed mandate into a vault token (the agent platform
// calling the PSP's delegate_payment).
type Delegator interface {
	Delegate(req acp.DelegateRequest) (token string, err error)
}

// Env is one agent session: tools bound to a user's wallet and shops.
type Env struct {
	Ctx       context.Context
	Wallet    *Wallet
	Shops     map[string]Shop // by merchant ID
	Delegator Delegator
	Log       []Call
}

// Call is one tool call, for transcripts and evaluation.
type Call struct {
	Tool   string
	Input  string
	Output string
}

// Tools describes the tools for an LLM (name, description, JSON schema).
var Tools = []struct {
	Name, Description string
	Schema            map[string]any
}{
	{"search_products", "Search a merchant's catalog. Returns products with id, name, description and unit_amount in paise.",
		obj(map[string]any{"merchant_id": str("merchant to search"), "query": str("words to match; empty lists everything")}, "merchant_id", "query")},
	{"create_checkout", "Create an ACP checkout session at a merchant. Returns the session with line items and totals in paise.",
		obj(map[string]any{"merchant_id": str("merchant"), "items": items()}, "merchant_id", "items")},
	{"update_checkout", "Replace the items of an open checkout session.",
		obj(map[string]any{"merchant_id": str("merchant"), "session_id": str("checkout session id"), "items": items()}, "merchant_id", "session_id", "items")},
	{"get_checkout", "Read a checkout session's current state.",
		obj(map[string]any{"merchant_id": str("merchant"), "session_id": str("checkout session id")}, "merchant_id", "session_id")},
	{"request_payment_approval", "Ask the user to approve paying up to max_amount (paise) for one checkout session by UPI. The user's phone signs the approval; you receive a one-time payment token, or the user's refusal.",
		obj(map[string]any{"merchant_id": str("merchant"), "session_id": str("checkout session id"), "max_amount": map[string]any{"type": "integer", "description": "most the user should approve, in paise"}}, "merchant_id", "session_id", "max_amount")},
	{"complete_checkout", "Pay for a checkout session with an approved payment token. Returns the session; status completed means paid and ordered.",
		obj(map[string]any{"merchant_id": str("merchant"), "session_id": str("checkout session id"), "token": str("payment token from request_payment_approval")}, "merchant_id", "session_id", "token")},
	{"cancel_checkout", "Cancel an open checkout session.",
		obj(map[string]any{"merchant_id": str("merchant"), "session_id": str("checkout session id")}, "merchant_id", "session_id")},
}

// Exec runs one tool call and returns its JSON result (errors as JSON too,
// so an agent can read them).
func (e *Env) Exec(tool string, input json.RawMessage) string {
	out := e.exec(tool, input)
	e.Log = append(e.Log, Call{tool, string(input), out})
	return out
}

func (e *Env) exec(tool string, input json.RawMessage) string {
	var in struct {
		MerchantID string     `json:"merchant_id"`
		Query      string     `json:"query"`
		SessionID  string     `json:"session_id"`
		Items      []acp.Item `json:"items"`
		MaxAmount  int64      `json:"max_amount"`
		Token      string     `json:"token"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return errJSON("invalid input: " + err.Error())
	}
	shop, ok := e.Shops[in.MerchantID]
	if !ok {
		return errJSON("unknown merchant_id " + in.MerchantID)
	}
	res := func(v any, err error) string {
		if err != nil {
			return errJSON(err.Error())
		}
		b, _ := json.Marshal(v)
		return string(b)
	}
	switch tool {
	case "search_products":
		return res(shop.Search(in.Query), nil)
	case "create_checkout":
		return res(shop.Create(in.Items))
	case "update_checkout":
		return res(shop.Update(in.SessionID, in.Items))
	case "get_checkout":
		return res(shop.Get(in.SessionID))
	case "complete_checkout":
		return res(shop.Complete(in.SessionID, in.Token))
	case "cancel_checkout":
		return res(shop.Cancel(in.SessionID))
	case "request_payment_approval":
		sm, err := e.Wallet.Approve(in.MerchantID, in.SessionID, money.Paise(in.MaxAmount))
		if err != nil {
			return res(map[string]any{"approved": false, "reason": err.Error()}, nil)
		}
		var req acp.DelegateRequest
		req.PaymentMethod.Type, req.PaymentMethod.VPA = acp.PaymentMethodType, e.Wallet.VPA
		req.Allowance = acp.Allowance{Reason: "one_time", MaxAmount: in.MaxAmount, Currency: "inr", CheckoutSessionID: in.SessionID, MerchantID: in.MerchantID, ExpiresAt: sm.Mandate.ExpiresAt}
		req.Metadata = map[string]string{"source": "agent"}
		req.UserMandate = &sm
		tok, err := e.Delegator.Delegate(req)
		if err != nil {
			return res(map[string]any{"approved": false, "reason": err.Error()}, nil)
		}
		return res(map[string]any{"approved": true, "token": tok, "max_amount": in.MaxAmount}, nil)
	}
	return errJSON("unknown tool " + tool)
}

// LocalShop calls a Merchant in-process.
type LocalShop struct{ M *acp.Merchant }

func (s LocalShop) MerchantID() string { return s.M.ID }
func (s LocalShop) Search(q string) []acp.Product {
	var out []acp.Product
	for _, p := range s.M.Catalog {
		if q == "" || strings.Contains(strings.ToLower(p.Name+" "+p.Description), strings.ToLower(q)) {
			out = append(out, p)
		}
	}
	return out
}
func (s LocalShop) Create(items []acp.Item) (acp.Session, error) {
	r, e := s.M.Create(items)
	return r, aerr(e)
}
func (s LocalShop) Update(id string, items []acp.Item) (acp.Session, error) {
	r, _, e := s.M.Update(id, items)
	return r, aerr(e)
}
func (s LocalShop) Get(id string) (acp.Session, error) {
	r, ok := s.M.Get(id)
	if !ok {
		return r, fmt.Errorf("checkout session not found")
	}
	return r, nil
}
func (s LocalShop) Complete(id, token string) (acp.Session, error) {
	var pd acp.PaymentData
	pd.HandlerID, pd.Instrument.Type = acp.HandlerID, acp.PaymentMethodType
	pd.Instrument.Credential.Type, pd.Instrument.Credential.Token = "vault_token", token
	r, _, e := s.M.Complete(context.Background(), id, pd)
	return r, aerr(e)
}
func (s LocalShop) Cancel(id string) (acp.Session, error) {
	r, _, e := s.M.Cancel(id)
	return r, aerr(e)
}

// LocalDelegator calls the PSP in-process.
type LocalDelegator struct{ P *acp.PSP }

func (d LocalDelegator) Delegate(req acp.DelegateRequest) (string, error) {
	t, _, e := d.P.Delegate(context.Background(), req)
	if e != nil {
		return "", fmt.Errorf("%s: %s", e.Code, e.Message)
	}
	return t.ID, nil
}

func aerr(e *acp.Error) error {
	if e == nil {
		return nil
	}
	return fmt.Errorf("%s: %s", e.Code, e.Message)
}

func errJSON(msg string) string {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return string(b)
}

func str(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
func items() map[string]any {
	return map[string]any{"type": "array", "items": obj(map[string]any{"id": str("product id"), "quantity": map[string]any{"type": "integer", "minimum": 1}}, "id", "quantity")}
}
func obj(props map[string]any, req ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": req, "additionalProperties": false}
}
