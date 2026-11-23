package acp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/chillprakash/upi-ledger/internal/bank"
	"github.com/chillprakash/upi-ledger/internal/clock"
	"github.com/chillprakash/upi-ledger/internal/ledger"
	"github.com/chillprakash/upi-ledger/internal/money"
	"github.com/chillprakash/upi-ledger/internal/npci"
)

// PaymentMethodType is this project's UPI payment method for ACP's
// delegate-payment API (the spec defines only "card").
const PaymentMethodType = "upi_reserve_pay"

// MaxAllowance is the longest an allowance may live.
const MaxAllowance = 48 * time.Hour

// DelegateRequest is the body of POST /agentic_commerce/delegate_payment.
type DelegateRequest struct {
	PaymentMethod struct {
		Type        string `json:"type"` // "upi_reserve_pay"
		VPA         string `json:"vpa"`
		DisplayName string `json:"display_name,omitempty"`
	} `json:"payment_method"`
	Allowance   Allowance         `json:"allowance"`
	RiskSignals []RiskSignal      `json:"risk_signals,omitempty"`
	Metadata    map[string]string `json:"metadata"`
	UserMandate *SignedMandate    `json:"user_mandate"`
}

// Allowance is ACP's constraint on how a delegated token may be used.
type Allowance struct {
	Reason            string `json:"reason"` // "one_time"
	MaxAmount         int64  `json:"max_amount"`
	Currency          string `json:"currency"`
	CheckoutSessionID string `json:"checkout_session_id"`
	MerchantID        string `json:"merchant_id"`
	ExpiresAt         string `json:"expires_at"`
}

// RiskSignal is accepted for shape compatibility (ACP's card_testing signal).
type RiskSignal struct {
	Type   string `json:"type"`
	Score  int    `json:"score"`
	Action string `json:"action"`
}

// DelegateResponse is the 201 body.
type DelegateResponse struct {
	ID       string            `json:"id"`
	Created  string            `json:"created"`
	Metadata map[string]string `json:"metadata"`
}

// Token is a delegated UPI allowance: a vault token bound to a Reserve Pay
// block at the payer's bank.
type Token struct {
	ID         string
	PayerVPA   string
	Bank       string
	MerchantID string
	Merchant   string // merchant UPI ID
	Max        money.Paise
	Session    string
	Expires    time.Time
	Block      string
	Mandate    SignedMandate
	Used       bool
	Captured   money.Paise // amount of the capture attempt
	TxnID      string
	Revoked    bool
	Released   bool
	State      string // issued, pending, captured, declined, revoked, expired
}

// CaptureStatus is the outcome of a capture.
type CaptureStatus string

const (
	CaptureSuccess  CaptureStatus = "success"
	CaptureDeclined CaptureStatus = "declined"
	CapturePending  CaptureStatus = "pending" // UPI outcome not final yet (deemed / unknown)
)

// CaptureResult is what the merchant gets back.
type CaptureResult struct {
	Status CaptureStatus
	TxnID  string
	Code   string
}

// Capture refusal codes.
const (
	CodeUnknownToken = "invalid_token"
	CodeRevoked      = "token_revoked"
	CodeExpired      = "token_expired"
	CodeWrongMerch   = "merchant_mismatch"
	CodeWrongSession = "checkout_session_mismatch"
	CodeOverLimit    = "amount_exceeds_allowance"
	CodeUsed         = "token_already_used"
	CodeBadAmount    = "invalid_amount"
)

// PSP is the payer's UPI app acting as the delegate-payment provider for
// agents. It owns user keys (registered at onboarding) and tokens.
type PSP struct {
	Switch    *npci.Switch
	Banks     map[string]*bank.Bank
	Clock     clock.Clock
	Keys      *Keyring
	Merchants map[string]string // ACP merchant_id -> merchant UPI ID

	mu     sync.Mutex
	tokens map[string]*Token
	seq    int
	// IDs, if set, makes token IDs deterministic (simulator).
	IDs func() string
}

// NewPSP returns a PSP.
func NewPSP(sw *npci.Switch, banks map[string]*bank.Bank, c clock.Clock, keys *Keyring, merchants map[string]string) *PSP {
	return &PSP{Switch: sw, Banks: banks, Clock: c, Keys: keys, Merchants: merchants, tokens: map[string]*Token{}}
}

// Handler serves POST /agentic_commerce/delegate_payment (wrap it in Middleware).
func (p *PSP) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agentic_commerce/delegate_payment", func(w http.ResponseWriter, r *http.Request) {
		var req DelegateRequest
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&req); err != nil {
			WriteError(w, 400, Error{Type: "invalid_request", Code: "invalid", Message: "invalid JSON body"})
			return
		}
		tok, status, e := p.Delegate(r.Context(), req)
		if e != nil {
			WriteError(w, status, *e)
			return
		}
		md := map[string]string{"merchant_id": tok.MerchantID, "idempotency_key": r.Header.Get("Idempotency-Key")}
		for k, v := range req.Metadata {
			if _, set := md[k]; !set {
				md[k] = v
			}
		}
		WriteJSON(w, http.StatusCreated, DelegateResponse{ID: tok.ID, Created: p.Clock.Now().UTC().Format(time.RFC3339), Metadata: md})
	})
	return mux
}

// Delegate validates a request and its signed mandate, then reserves the
// allowance as a Reserve Pay block at the payer's bank.
func (p *PSP) Delegate(ctx context.Context, req DelegateRequest) (*Token, int, *Error) {
	bad := func(code, msg, param string) (*Token, int, *Error) {
		return nil, 400, &Error{Type: "invalid_request", Code: code, Message: msg, Param: param}
	}
	a := req.Allowance
	now := p.Clock.Now()
	switch {
	case req.PaymentMethod.Type != PaymentMethodType:
		return bad("invalid", "payment_method.type must be "+PaymentMethodType, "$.payment_method.type")
	case req.PaymentMethod.VPA == "":
		return bad("missing", "payment_method.vpa is required", "$.payment_method.vpa")
	case a.Reason != "one_time":
		return bad("invalid", "allowance.reason must be one_time", "$.allowance.reason")
	case a.Currency != "inr":
		return bad("invalid", "allowance.currency must be inr", "$.allowance.currency")
	case a.MaxAmount <= 0:
		return bad("invalid", "allowance.max_amount must be a positive integer in paise", "$.allowance.max_amount")
	case a.CheckoutSessionID == "":
		return bad("missing", "allowance.checkout_session_id is required", "$.allowance.checkout_session_id")
	case req.Metadata == nil:
		return bad("missing", "metadata is required", "$.metadata")
	case req.UserMandate == nil:
		return nil, 401, &Error{Type: "invalid_request", Code: "mandate_required", Message: "a signed user mandate is required", Param: "$.user_mandate"}
	}
	merchantVPA, ok := p.Merchants[a.MerchantID]
	if !ok {
		return bad("invalid", "unknown merchant_id", "$.allowance.merchant_id")
	}
	exp, err := time.Parse(time.RFC3339, a.ExpiresAt)
	if err != nil || !exp.After(now) || exp.Sub(now) > MaxAllowance {
		return bad("invalid", "allowance.expires_at must be in the future and within 48 hours", "$.allowance.expires_at")
	}
	m := req.UserMandate.Mandate
	if m.PayerVPA != req.PaymentMethod.VPA || m.MerchantID != a.MerchantID || m.MaxAmount != a.MaxAmount ||
		m.Currency != a.Currency || m.CheckoutSessionID != a.CheckoutSessionID || m.ExpiresAt != a.ExpiresAt {
		return nil, 422, &Error{Type: "invalid_request", Code: "mandate_mismatch", Message: ErrMandateMismatch.Error(), Param: "$.user_mandate.mandate"}
	}
	if err := p.Keys.Verify(*req.UserMandate, now); err != nil {
		return nil, 401, &Error{Type: "invalid_request", Code: "mandate_invalid", Message: err.Error(), Param: "$.user_mandate"}
	}
	addr, err := p.Switch.ValidateAddress(req.PaymentMethod.VPA)
	if err != nil {
		return bad("invalid", "payer UPI ID not found", "$.payment_method.vpa")
	}
	bk := p.Banks[addr.Bank]

	p.mu.Lock()
	defer p.mu.Unlock()
	// A new mandate for the same payer and merchant supersedes an unused
	// earlier allowance (Reserve Pay allows one active block per merchant).
	for _, t := range p.sortedTokens() {
		if t.PayerVPA == m.PayerVPA && t.Merchant == merchantVPA && !t.Used && !t.Revoked && !t.Released {
			p.release(ctx, t, "revoked")
		}
	}
	id := p.newID()
	t := &Token{ID: id, PayerVPA: m.PayerVPA, Bank: addr.Bank, MerchantID: a.MerchantID, Merchant: merchantVPA,
		Max: money.Paise(a.MaxAmount), Session: a.CheckoutSessionID, Expires: exp, Block: "acp-" + id, Mandate: *req.UserMandate, State: "issued"}
	if err := bk.CreateBlock(ctx, t.Block, addr.Account, merchantVPA, t.Max, exp); err != nil {
		code, status := "processing_error", 500
		if errors.Is(err, ledger.ErrInsufficientFunds) {
			code, status = "insufficient_funds", 422
		}
		return nil, status, &Error{Type: map[int]string{422: "invalid_request", 500: "processing_error"}[status], Code: code, Message: err.Error()}
	}
	p.tokens[id] = t
	return t, 201, nil
}

// Capture debits a token's block for a checkout. Every rule is checked here,
// whatever the agent or merchant claims: the token must exist, be unrevoked
// and unexpired, belong to this merchant and checkout, and not exceed or
// reuse the allowance. A repeat of the same capture returns its status.
func (p *PSP) Capture(ctx context.Context, tokenID, merchantID, session string, amount money.Paise) CaptureResult {
	p.mu.Lock()
	t, ok := p.tokens[tokenID]
	refuse := func(code string) CaptureResult {
		p.mu.Unlock()
		return CaptureResult{Status: CaptureDeclined, Code: code}
	}
	switch {
	case !ok:
		return refuse(CodeUnknownToken)
	case t.MerchantID != merchantID:
		return refuse(CodeWrongMerch)
	case t.Session != session:
		return refuse(CodeWrongSession)
	case t.Used:
		txn, same := t.TxnID, t.Captured == amount
		p.mu.Unlock()
		if !same {
			return CaptureResult{Status: CaptureDeclined, Code: CodeUsed}
		}
		return p.status(ctx, tokenID, txn)
	case t.Revoked || t.Released:
		return refuse(CodeRevoked)
	case !p.Clock.Now().Before(t.Expires):
		return refuse(CodeExpired)
	case amount <= 0:
		return refuse(CodeBadAmount)
	case amount > t.Max:
		// The bound merchant's checkout costs more than the user approved:
		// this token can never complete it, so free the reservation now
		// instead of leaving the user's money blocked until expiry.
		t.State = "declined"
		p.release(ctx, t, "declined")
		return refuse(CodeOverLimit)
	}
	t.Used, t.Captured, t.TxnID, t.State = true, amount, "ACP"+t.ID, "pending"
	req := npci.PayRequest{TxnID: t.TxnID, Payer: t.PayerVPA, Payee: t.Merchant, Amount: amount, Merchant: true, Block: t.Block}
	p.mu.Unlock()

	if _, err := p.Switch.Pay(ctx, req); err != nil {
		return CaptureResult{Status: CaptureDeclined, TxnID: req.TxnID, Code: err.Error()}
	}
	return p.status(ctx, tokenID, req.TxnID)
}

// status maps the switch's record to a capture status and releases the
// rest of the block once the outcome is final.
func (p *PSP) status(ctx context.Context, tokenID, txn string) CaptureResult {
	r, err := p.Switch.CheckStatus(txn)
	if err != nil {
		return CaptureResult{Status: CapturePending, TxnID: txn}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.tokens[tokenID]
	switch {
	case r.Status == npci.Success && !r.ReversalDue:
		t.State = "captured"
		p.release(ctx, t, "captured")
		return CaptureResult{Status: CaptureSuccess, TxnID: txn, Code: r.Code}
	case (r.Status == npci.Failed || r.Status == npci.Reversed) && !r.ReversalDue:
		t.State = "declined"
		p.release(ctx, t, "declined")
		return CaptureResult{Status: CaptureDeclined, TxnID: txn, Code: r.Code}
	}
	return CaptureResult{Status: CapturePending, TxnID: txn}
}

// Revoke is the user withdrawing an allowance before it is used.
func (p *PSP) Revoke(ctx context.Context, tokenID, payerVPA string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	t, ok := p.tokens[tokenID]
	if !ok || t.PayerVPA != payerVPA {
		return errors.New("acp: no such allowance for this user")
	}
	if t.Used {
		return errors.New("acp: allowance already used")
	}
	t.Revoked = true
	p.release(ctx, t, "revoked")
	return nil
}

// Sweep expires unused allowances and settles pending captures. Call it as
// time passes.
func (p *PSP) Sweep(ctx context.Context) {
	now := p.Clock.Now()
	p.mu.Lock()
	var pending []*Token
	for _, t := range p.sortedTokens() {
		switch {
		case !t.Used && !t.Released && !now.Before(t.Expires):
			p.release(ctx, t, "expired")
		case t.Used && t.State == "pending":
			pending = append(pending, t)
		}
	}
	p.mu.Unlock()
	for _, t := range pending {
		p.status(ctx, t.ID, t.TxnID)
	}
}

// CaptureStatusOf reports a token's capture state (for merchants
// reconciling in-progress checkouts).
func (p *PSP) CaptureStatusOf(ctx context.Context, tokenID string) CaptureResult {
	p.mu.Lock()
	t, ok := p.tokens[tokenID]
	if !ok || !t.Used {
		p.mu.Unlock()
		return CaptureResult{Status: CaptureDeclined, Code: CodeUnknownToken}
	}
	txn := t.TxnID
	p.mu.Unlock()
	return p.status(ctx, tokenID, txn)
}

// Tokens returns copies of all tokens sorted by ID.
func (p *PSP) Tokens() []Token {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Token, 0, len(p.tokens))
	for _, t := range p.sortedTokens() {
		out = append(out, *t)
	}
	return out
}

// release ends the token's block (what is left of it); p.mu held.
func (p *PSP) release(ctx context.Context, t *Token, state string) {
	if t.Released {
		return
	}
	if b, ok := p.Banks[t.Bank].GetBlock(t.Block); ok && b.Status == "active" {
		_ = p.Banks[t.Bank].RevokeBlock(ctx, t.Block)
	}
	t.Released = true
	if state == "revoked" {
		t.Revoked = true
	}
	if t.State == "issued" {
		t.State = state
	}
}

func (p *PSP) sortedTokens() []*Token {
	out := make([]*Token, 0, len(p.tokens))
	for _, t := range p.tokens {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (p *PSP) newID() string {
	if p.IDs != nil {
		return p.IDs()
	}
	b := make([]byte, 10)
	rand.Read(b) //nolint:errcheck
	return "vt_upi_" + hex.EncodeToString(b)
}
