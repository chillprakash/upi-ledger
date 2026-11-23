package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"

	"github.com/chillprakash/upi-ledger/internal/clock"
	"github.com/chillprakash/upi-ledger/internal/money"
)

// HandlerID is the merchant's payment handler for UPI allowances.
const HandlerID = "upi_reserve_pay"

// Product is a catalog item.
type Product struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	UnitAmount  int64  `json:"unit_amount"` // paise
}

// Item is an ACP item reference.
type Item struct {
	ID       string `json:"id"`
	Quantity int    `json:"quantity"`
}

// LineItem is an ACP line item (integer minor units).
type LineItem struct {
	ID         string `json:"id"`
	Item       Item   `json:"item"`
	Quantity   int    `json:"quantity"`
	Name       string `json:"name,omitempty"`
	UnitAmount int64  `json:"unit_amount"`
	BaseAmount int64  `json:"base_amount"`
	Discount   int64  `json:"discount"`
	Subtotal   int64  `json:"subtotal"`
	Tax        int64  `json:"tax"`
	Total      int64  `json:"total"`
}

// Total is an ACP total.
type Total struct {
	Type        string `json:"type"`
	DisplayText string `json:"display_text"`
	Amount      int64  `json:"amount"`
}

// Message is an ACP message.
type Message struct {
	Type        string `json:"type"` // info | warning | error
	Code        string `json:"code,omitempty"`
	Resolution  string `json:"resolution,omitempty"`
	ContentType string `json:"content_type"`
	Content     string `json:"content"`
}

// PaymentHandler is an ACP payment handler advertisement.
type PaymentHandler struct {
	ID                      string         `json:"id"`
	Name                    string         `json:"name"`
	DisplayName             string         `json:"display_name"`
	Version                 string         `json:"version"`
	Spec                    string         `json:"spec"`
	RequiresDelegatePayment bool           `json:"requires_delegate_payment"`
	RequiresPCICompliance   bool           `json:"requires_pci_compliance"`
	PSP                     string         `json:"psp"`
	InstrumentSchemas       []string       `json:"instrument_schemas"`
	Config                  map[string]any `json:"config"`
}

// Order is created when a checkout completes.
type Order struct {
	ID                string `json:"id"`
	CheckoutSessionID string `json:"checkout_session_id"`
	PermalinkURL      string `json:"permalink_url"`
}

// Session is an ACP checkout session.
type Session struct {
	ID           string `json:"id"`
	Capabilities struct {
		Payment struct {
			Handlers []PaymentHandler `json:"handlers"`
		} `json:"payment"`
	} `json:"capabilities"`
	Status    string     `json:"status"` // not_ready_for_payment | ready_for_payment | in_progress | completed | canceled
	Currency  string     `json:"currency"`
	LineItems []LineItem `json:"line_items"`
	Totals    []Total    `json:"totals"`
	Messages  []Message  `json:"messages"`
	Links     []any      `json:"links"`
	Order     *Order     `json:"order,omitempty"`

	token string // vault token used to pay (internal)
}

// PaymentData is the payment block of a complete request.
type PaymentData struct {
	HandlerID  string `json:"handler_id"`
	Instrument struct {
		Type       string `json:"type"`
		Credential struct {
			Type  string `json:"type"` // "vault_token"
			Token string `json:"token"`
		} `json:"credential"`
	} `json:"instrument"`
}

// itemsBody accepts both forms seen in ACP: "items" (RFC text) and
// "line_items" with an id and optional quantity (2026-04-17 examples).
type itemsBody struct {
	Items     []Item `json:"items"`
	LineItems []struct {
		ID       string `json:"id"`
		Quantity int    `json:"quantity"`
	} `json:"line_items"`
}

func (b itemsBody) list() []Item {
	if len(b.Items) > 0 {
		return b.Items
	}
	var out []Item
	for _, l := range b.LineItems {
		q := l.Quantity
		if q == 0 {
			q = 1
		}
		out = append(out, Item{ID: l.ID, Quantity: q})
	}
	return out
}

// Event is an order webhook event (order_create / order_update).
type Event struct {
	Type    string
	Session string
	Order   string
}

// Merchant serves ACP's checkout API for one business that accepts UPI
// allowances.
type Merchant struct {
	ID       string // ACP merchant_id
	VPA      string
	Catalog  map[string]Product
	TaxBPS   int64 // tax in basis points
	PSP      *PSP
	Clock    clock.Clock
	BaseURL  string
	mu       sync.Mutex
	sessions map[string]*Session
	seq      int
	events   []Event
}

// NewMerchant returns a merchant.
func NewMerchant(id, vpa string, catalog []Product, psp *PSP, c clock.Clock) *Merchant {
	m := &Merchant{ID: id, VPA: vpa, Catalog: map[string]Product{}, TaxBPS: 500, PSP: psp, Clock: c, BaseURL: "https://" + id + ".example", sessions: map[string]*Session{}}
	for _, p := range catalog {
		m.Catalog[p.ID] = p
	}
	return m
}

// Handler serves the five checkout endpoints (wrap it in Middleware).
func (m *Merchant) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /checkout_sessions", func(w http.ResponseWriter, r *http.Request) {
		var body itemsBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, 400, Error{Type: "invalid_request", Code: "invalid", Message: "invalid JSON body"})
			return
		}
		s, e := m.Create(body.list())
		if e != nil {
			WriteError(w, 400, *e)
			return
		}
		WriteJSON(w, http.StatusCreated, s)
	})
	mux.HandleFunc("GET /checkout_sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		s, ok := m.Get(r.PathValue("id"))
		if !ok {
			WriteError(w, 404, Error{Type: "invalid_request", Code: "not_found", Message: "checkout session not found"})
			return
		}
		WriteJSON(w, 200, s)
	})
	mux.HandleFunc("POST /checkout_sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body itemsBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, 400, Error{Type: "invalid_request", Code: "invalid", Message: "invalid JSON body"})
			return
		}
		s, status, e := m.Update(r.PathValue("id"), body.list())
		if e != nil {
			WriteError(w, status, *e)
			return
		}
		WriteJSON(w, 200, s)
	})
	mux.HandleFunc("POST /checkout_sessions/{id}/complete", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			PaymentData PaymentData `json:"payment_data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, 400, Error{Type: "invalid_request", Code: "invalid", Message: "invalid JSON body"})
			return
		}
		s, status, e := m.Complete(r.Context(), r.PathValue("id"), body.PaymentData)
		if e != nil {
			WriteError(w, status, *e)
			return
		}
		WriteJSON(w, 200, s)
	})
	mux.HandleFunc("POST /checkout_sessions/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		s, status, e := m.Cancel(r.PathValue("id"))
		if e != nil {
			WriteError(w, status, *e)
			return
		}
		WriteJSON(w, 200, s)
	})
	return mux
}

// Create starts a checkout session.
func (m *Merchant) Create(items []Item) (Session, *Error) {
	lines, e := m.price(items)
	if e != nil {
		return Session{}, e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	s := &Session{ID: fmt.Sprintf("cs_%s_%06d", m.ID, m.seq), Currency: "inr", Status: "ready_for_payment", Links: []any{}}
	s.Capabilities.Payment.Handlers = []PaymentHandler{m.handler()}
	m.setLines(s, lines)
	m.sessions[s.ID] = s
	return clone(s), nil
}

// Get returns a session.
func (m *Merchant) Get(id string) (Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return Session{}, false
	}
	return clone(s), true
}

// Update replaces the items of an open session.
func (m *Merchant) Update(id string, items []Item) (Session, int, *Error) {
	lines, e := m.price(items)
	if e != nil {
		return Session{}, 400, e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return Session{}, 404, &Error{Type: "invalid_request", Code: "not_found", Message: "checkout session not found"}
	}
	if s.Status != "ready_for_payment" && s.Status != "not_ready_for_payment" {
		return Session{}, 405, &Error{Type: "invalid_request", Code: "invalid_state", Message: "session can no longer be changed (" + s.Status + ")"}
	}
	m.setLines(s, lines)
	return clone(s), 200, nil
}

// Complete pays for a session with a UPI allowance and creates the order.
// The session moves to in_progress before the capture, so a second
// complete (any key) can never capture twice.
func (m *Merchant) Complete(ctx context.Context, id string, pd PaymentData) (Session, int, *Error) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	switch {
	case !ok:
		m.mu.Unlock()
		return Session{}, 404, &Error{Type: "invalid_request", Code: "not_found", Message: "checkout session not found"}
	case s.Status == "completed" || s.Status == "in_progress":
		out := clone(s)
		m.mu.Unlock()
		return out, 200, nil
	case s.Status != "ready_for_payment":
		m.mu.Unlock()
		return Session{}, 405, &Error{Type: "invalid_request", Code: "invalid_state", Message: "session is " + s.Status}
	case pd.HandlerID != HandlerID || pd.Instrument.Type != PaymentMethodType || pd.Instrument.Credential.Type != "vault_token" || pd.Instrument.Credential.Token == "":
		m.mu.Unlock()
		return Session{}, 400, &Error{Type: "invalid_request", Code: "invalid", Message: "payment_data must use handler " + HandlerID + " with a vault_token credential", Param: "$.payment_data"}
	}
	s.Status, s.token = "in_progress", pd.Instrument.Credential.Token
	total := grandTotal(s)
	m.mu.Unlock()

	res := m.PSP.Capture(ctx, s.token, m.ID, id, money.Paise(total))
	m.mu.Lock()
	defer m.mu.Unlock()
	m.apply(s, res)
	return clone(s), 200, nil
}

// Reconcile settles in-progress sessions whose UPI outcome was uncertain.
func (m *Merchant) Reconcile(ctx context.Context) {
	m.mu.Lock()
	var open []*Session
	for _, s := range m.sessions {
		if s.Status == "in_progress" {
			open = append(open, s)
		}
	}
	m.mu.Unlock()
	sort.Slice(open, func(i, j int) bool { return open[i].ID < open[j].ID })
	for _, s := range open {
		res := m.PSP.CaptureStatusOf(ctx, s.token)
		m.mu.Lock()
		m.apply(s, res)
		m.mu.Unlock()
	}
}

// apply records a capture result on a session; m.mu held.
func (m *Merchant) apply(s *Session, res CaptureResult) {
	if s.Status != "in_progress" {
		return
	}
	switch res.Status {
	case CaptureSuccess:
		s.Status = "completed"
		s.Order = &Order{ID: "ord_" + s.ID[3:], CheckoutSessionID: s.ID, PermalinkURL: m.BaseURL + "/orders/" + s.ID}
		s.Messages = []Message{{Type: "info", ContentType: "plain", Content: "Paid by UPI (" + res.TxnID + ")."}}
		m.events = append(m.events, Event{"order_create", s.ID, s.Order.ID})
	case CaptureDeclined:
		s.Status, s.token = "ready_for_payment", ""
		s.Messages = []Message{{Type: "error", Code: "payment_declined", Resolution: "requires_buyer_input", ContentType: "plain", Content: "UPI payment declined (" + res.Code + "). Ask the buyer to approve a new payment."}}
	default:
		s.Messages = []Message{{Type: "info", ContentType: "plain", Content: "UPI payment is being confirmed; the order is created when it settles."}}
	}
}

// Cancel cancels a session not yet completed or canceled (405 otherwise).
func (m *Merchant) Cancel(id string) (Session, int, *Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return Session{}, 404, &Error{Type: "invalid_request", Code: "not_found", Message: "checkout session not found"}
	}
	if s.Status == "completed" || s.Status == "canceled" || s.Status == "in_progress" {
		return Session{}, 405, &Error{Type: "invalid_request", Code: "invalid_state", Message: "session is " + s.Status}
	}
	s.Status = "canceled"
	return clone(s), 200, nil
}

// Sessions returns all sessions sorted by ID.
func (m *Merchant) Sessions() []Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, clone(s))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Events returns emitted order events.
func (m *Merchant) Events() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Event(nil), m.events...)
}

// TokenOf returns the vault token a session was paid with.
func (m *Merchant) TokenOf(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok {
		return s.token
	}
	return ""
}

func (m *Merchant) price(items []Item) ([]LineItem, *Error) {
	if len(items) == 0 {
		return nil, &Error{Type: "invalid_request", Code: "missing", Message: "at least one item is required", Param: "$.items"}
	}
	var out []LineItem
	for i, it := range items {
		p, ok := m.Catalog[it.ID]
		if !ok {
			return nil, &Error{Type: "invalid_request", Code: "invalid_item_id", Message: fmt.Sprintf("The item ID '%s' does not exist.", it.ID), Param: fmt.Sprintf("$.line_items[%d].item.id", i)}
		}
		if it.Quantity < 1 || it.Quantity > 100 {
			return nil, &Error{Type: "invalid_request", Code: "invalid", Message: "quantity must be 1-100", Param: fmt.Sprintf("$.items[%d].quantity", i)}
		}
		base := p.UnitAmount * int64(it.Quantity)
		tax := base * m.TaxBPS / 10000
		out = append(out, LineItem{ID: fmt.Sprintf("li_%d", i+1), Item: it, Quantity: it.Quantity, Name: p.Name, UnitAmount: p.UnitAmount, BaseAmount: base, Subtotal: base, Tax: tax, Total: base + tax})
	}
	return out, nil
}

func (m *Merchant) setLines(s *Session, lines []LineItem) {
	var base, tax int64
	for _, l := range lines {
		base += l.BaseAmount
		tax += l.Tax
	}
	s.LineItems = lines
	s.Totals = []Total{
		{"items_base_amount", "Items", base},
		{"subtotal", "Subtotal", base},
		{"tax", "GST", tax},
		{"total", "Total", base + tax},
	}
	s.Messages = nil
}

func (m *Merchant) handler() PaymentHandler {
	return PaymentHandler{
		ID: HandlerID, Name: "io.github.chillprakash.upi.reserve_pay", DisplayName: "UPI (Reserve Pay)", Version: "2026-11-23",
		Spec:                    "https://github.com/chillprakash/upi-ledger/blob/main/docs/acp-upi-handler.md",
		RequiresDelegatePayment: true, RequiresPCICompliance: false, PSP: "upi-ledger",
		InstrumentSchemas: []string{"https://github.com/chillprakash/upi-ledger/blob/main/docs/acp-upi-handler.md#instrument"},
		Config:            map[string]any{"merchant_id": m.ID, "merchant_vpa": m.VPA, "currency": "inr", "max_allowance_hours": 48},
	}
}

func grandTotal(s *Session) int64 {
	for _, t := range s.Totals {
		if t.Type == "total" {
			return t.Amount
		}
	}
	return 0
}

func clone(s *Session) Session {
	c := *s
	c.LineItems = append([]LineItem(nil), s.LineItems...)
	c.Totals = append([]Total(nil), s.Totals...)
	c.Messages = append([]Message(nil), s.Messages...)
	if s.Order != nil {
		o := *s.Order
		c.Order = &o
	}
	return c
}
