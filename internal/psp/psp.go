// Package psp is the app side of UPI: the payer PSP / UPI app (the seat a
// TPAP like WhatsApp sits in). It takes payment requests from users,
// deduplicates them with idempotency keys, sends ReqPay to the switch, and
// follows up on uncertain outcomes under NPCI's status-check rules. It keeps
// payment state only, never account balances (Consolidated Circular 12.8).
//
// Rules implemented (UPI Consolidated Circular, 18 Sep 2026):
//   - 2.13   payee's CBS name resolved and shown before authorisation.
//   - 2.16   no collect requests for person-to-person payments.
//   - 12.8   status check only after 60 s, at most 3, within 2 h.
//   - 12.4   U48 (not found) -> wait for the settlement file.
//   - 12.5 / Annexure C  an Annexure C error (e.g. connection failure) means
//     no further status checks and the payment is treated as failed. Where
//     that error came from a status check rather than the original request,
//     the money may in fact have moved, so the payment is flagged for
//     reconciliation against the settlement file, which has the final word.
package psp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/chillprakash/upi-ledger/internal/clock"
	"github.com/chillprakash/upi-ledger/internal/money"
	"github.com/chillprakash/upi-ledger/internal/netsim"
	"github.com/chillprakash/upi-ledger/internal/npci"
)

// Status-check schedule, measured from initiation (12.8).
var CheckOffsets = []time.Duration{60 * time.Second, 10 * time.Minute, 60 * time.Minute}

// CheckWindow is how long after initiation status checks are allowed.
const CheckWindow = 2 * time.Hour

// State of a payment as the app sees it.
type State string

const (
	Initiated          State = "initiated"
	Pending            State = "pending"
	Deemed             State = "deemed"
	Success            State = "success"
	Failed             State = "failed"
	Reversed           State = "reversed"
	AwaitingSettlement State = "awaiting_settlement"
)

// Final reports whether the app will do nothing more on its own.
func (s State) Final() bool { return s == Success || s == Failed || s == Reversed }

// Kind of payment.
type Kind string

const (
	Push    Kind = "push"
	Collect Kind = "collect"
)

// Request is what a user asks for.
type Request struct {
	Payer    string
	Payee    string
	Amount   money.Paise
	Kind     Kind
	Merchant bool   // payee is a merchant (collect allowed only then)
	Lite     bool   // pay from UPI LITE (see ChooseLite)
	Block    string // Reserve Pay block to debit (merchant-initiated)
}

func (r Request) fingerprint() string {
	return fmt.Sprintf("%s|%s|%d|%s|%v|%v|%s", r.Payer, r.Payee, r.Amount, r.Kind, r.Merchant, r.Lite, r.Block)
}

// Payment is the app's record.
type Payment struct {
	ID             string
	IdempotencyKey string
	TxnID          string
	Request        Request
	PayeeName      string
	State          State
	Code           string
	Checks         int
	NextCheck      time.Time
	ReconRequired  bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
	History        []string
}

// Errors returned to callers.
var (
	ErrIdempotencyMismatch = errors.New("psp: idempotency key reused with a different request")
	ErrP2PCollect          = errors.New("psp: collect is not allowed for person-to-person payments (2.16)")
	ErrInvalid             = errors.New("psp: invalid request")
	ErrTryAgain            = errors.New("psp: switch unreachable, try again")
	ErrUnknownPayee        = errors.New("psp: payee UPI ID not found")
	// ErrCrashed is returned when a simulated crash stops the engine mid-call.
	ErrCrashed = errors.New("psp: engine crashed (simulated)")
)

// Stats counts what the engine did, for the simulator's report.
type Stats struct {
	StatusChecks      int
	ChecksSkippedRule int // a check was due but rules said no (count/window)
	ReconFlagged      int
	ReconCorrections  map[string]int // "from->to"
	PersistErrors     int
	Recovered         int // payments found mid-flight at restart
}

// Engine is the payer PSP.
type Engine struct {
	Switch *npci.Switch
	Net    *netsim.Net
	Clock  clock.Clock
	Prefix string // transaction ID prefix, e.g. "WAP"
	Store  Store
	// Crash, if set, is asked at "after_insert" (payment saved, ReqPay not
	// sent) and "after_send" (ReqPay sent, result not saved). Returning true
	// simulates the process dying there.
	Crash func(point string) bool

	mu       sync.Mutex
	seq      int
	payments map[string]*Payment // by ID
	byKey    map[string]string   // idempotency key -> payment ID
	stats    Stats
}

// New returns an engine.
func New(sw *npci.Switch, net *netsim.Net, c clock.Clock, prefix string) *Engine {
	return &Engine{Switch: sw, Net: net, Clock: c, Prefix: prefix, Store: NewMemStore(), payments: map[string]*Payment{}, byKey: map[string]string{}, stats: Stats{ReconCorrections: map[string]int{}}}
}

// Restore loads payments from the store after a restart. A payment still
// "initiated" was saved before a crash: ReqPay may or may not have reached
// the switch, so it becomes pending and status checks find out (the switch
// answers U48 if it never arrived, and reconciliation settles it).
func (e *Engine) Restore(ctx context.Context) error {
	ps, err := e.Store.Load(ctx)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range ps {
		p := ps[i]
		var n int
		if _, err := fmt.Sscanf(p.ID, "pay_%d", &n); err == nil && n > e.seq {
			e.seq = n
		}
		if p.State == Initiated {
			e.stats.Recovered++
			e.set(&p, Pending, "RECOVERED")
			p.NextCheck = p.CreatedAt.Add(CheckOffsets[0])
			e.persist(&p)
		}
		e.payments[p.ID] = &p
		e.byKey[p.IdempotencyKey] = p.ID
	}
	return nil
}

// persist writes a payment through to the store. A failed write leaves the
// store one step behind; after a restart that payment is re-checked, which
// is safe because the switch is idempotent and reconciliation has the last word.
func (e *Engine) persist(p *Payment) {
	if err := e.Store.Update(context.Background(), clone(p)); err != nil {
		e.stats.PersistErrors++
	}
}

// Create starts a payment. The same idempotency key with the same request
// returns the existing payment; with a different request it is rejected.
func (e *Engine) Create(ctx context.Context, key string, req Request) (Payment, error) {
	if key == "" || req.Payer == "" || req.Payee == "" || req.Payer == req.Payee || req.Amount <= 0 {
		return Payment{}, ErrInvalid
	}
	if req.Kind == "" {
		req.Kind = Push
	}
	if req.Kind == Collect && !req.Merchant {
		return Payment{}, ErrP2PCollect
	}
	e.mu.Lock()
	if id, ok := e.byKey[key]; ok {
		p := e.payments[id]
		defer e.mu.Unlock()
		if p.Request.fingerprint() != req.fingerprint() {
			return Payment{}, ErrIdempotencyMismatch
		}
		return clone(p), nil
	}
	e.mu.Unlock()

	addr, err := netsim.Call(e.Net, "psp>npci", npci.StatusDeadline, func() result[npci.Address] {
		a, err := e.Switch.ValidateAddress(req.Payee)
		return result[npci.Address]{a, err}
	})
	if err != nil {
		return Payment{}, ErrTryAgain
	}
	if addr.err != nil {
		return Payment{}, ErrUnknownPayee
	}

	e.mu.Lock()
	if id, ok := e.byKey[key]; ok { // a concurrent duplicate won the race
		p := e.payments[id]
		defer e.mu.Unlock()
		if p.Request.fingerprint() != req.fingerprint() {
			return Payment{}, ErrIdempotencyMismatch
		}
		return clone(p), nil
	}
	e.seq++
	now := e.Clock.Now()
	p := &Payment{
		ID: fmt.Sprintf("pay_%08d", e.seq), IdempotencyKey: key,
		TxnID:   fmt.Sprintf("%s%029d", e.Prefix, e.seq), // 32+ chars, like UPI txn IDs
		Request: req, PayeeName: addr.v.Name, State: Initiated, CreatedAt: now, UpdatedAt: now,
	}
	// Stored with its key before ReqPay is sent: a retry can never mint a
	// second transaction for the same user intent.
	stored, inserted, err := e.Store.Insert(ctx, clone(p))
	if err != nil {
		e.seq--
		e.mu.Unlock()
		return Payment{}, err
	}
	if !inserted { // another process holds this key
		defer e.mu.Unlock()
		e.seq--
		e.payments[stored.ID] = &stored
		e.byKey[key] = stored.ID
		if stored.Request.fingerprint() != req.fingerprint() {
			return Payment{}, ErrIdempotencyMismatch
		}
		return clone(&stored), nil
	}
	e.payments[p.ID] = p
	e.byKey[key] = p.ID
	e.mu.Unlock()
	if e.Crash != nil && e.Crash("after_insert") {
		return Payment{}, ErrCrashed
	}

	rec, err := netsim.Call(e.Net, "psp>npci", npci.PayDeadline, func() result[npci.Record] {
		r, err := e.Switch.Pay(ctx, npci.PayRequest{TxnID: p.TxnID, Payer: req.Payer, Payee: req.Payee, Amount: req.Amount, Merchant: req.Merchant, Lite: req.Lite, Block: req.Block})
		return result[npci.Record]{r, err}
	})
	if e.Crash != nil && e.Crash("after_send") {
		return Payment{}, ErrCrashed
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.persist(p)
	switch {
	case errors.Is(err, netsim.ErrConnect):
		// Annexure C: never reached NPCI; failed, no status check.
		e.set(p, Failed, "CONNECT_FAILED")
	case err != nil:
		e.set(p, Pending, "TIMEOUT")
		p.NextCheck = p.CreatedAt.Add(CheckOffsets[0])
	case rec.err != nil:
		e.set(p, Failed, "REJECTED:"+rec.err.Error())
	default:
		e.apply(p, rec.v)
	}
	return clone(p), nil
}

// Tick runs status checks that are due. Call it as time passes.
func (e *Engine) Tick(ctx context.Context) {
	now := e.Clock.Now()
	e.mu.Lock()
	var due []*Payment
	for _, p := range e.payments {
		if (p.State == Pending || p.State == Deemed) && !p.NextCheck.After(now) {
			due = append(due, p)
		}
	}
	e.mu.Unlock()
	sort.Slice(due, func(i, j int) bool { return due[i].ID < due[j].ID })
	for _, p := range due {
		e.check(ctx, p)
	}
}

func (e *Engine) check(ctx context.Context, p *Payment) {
	e.mu.Lock()
	now := e.Clock.Now()
	if p.Checks >= len(CheckOffsets) || now.Sub(p.CreatedAt) > CheckWindow {
		e.stats.ChecksSkippedRule++
		e.set(p, AwaitingSettlement, "CHECKS_EXHAUSTED")
		e.persist(p)
		e.mu.Unlock()
		return
	}
	if now.Sub(p.CreatedAt) < CheckOffsets[0] { // never before 60 s
		p.NextCheck = p.CreatedAt.Add(CheckOffsets[0])
		e.mu.Unlock()
		return
	}
	p.Checks++
	e.stats.StatusChecks++
	txn := p.TxnID
	e.persist(p) // the check is counted before it is sent: a crash never earns an extra check
	e.mu.Unlock()

	rec, err := netsim.Call(e.Net, "psp>npci", npci.StatusDeadline, func() result[npci.Record] {
		r, err := e.Switch.CheckStatus(txn)
		return result[npci.Record]{r, err}
	})

	e.mu.Lock()
	defer e.mu.Unlock()
	defer e.persist(p)
	switch {
	case errors.Is(err, netsim.ErrConnect):
		// 12.5 / Annexure C, applied literally: stop checking, treat as failed.
		// The money may have moved, so reconciliation must confirm.
		p.ReconRequired = true
		e.stats.ReconFlagged++
		e.set(p, Failed, "CHECK_CONNECT_FAILED")
	case err != nil:
		e.scheduleNext(p)
	case errors.Is(rec.err, npci.ErrNotFound):
		e.set(p, AwaitingSettlement, "U48")
	case rec.err != nil:
		e.scheduleNext(p)
	default:
		e.apply(p, rec.v)
	}
}

func (e *Engine) scheduleNext(p *Payment) {
	if p.Checks < len(CheckOffsets) {
		p.NextCheck = p.CreatedAt.Add(CheckOffsets[p.Checks])
		return
	}
	e.set(p, AwaitingSettlement, "CHECKS_EXHAUSTED")
}

// apply maps the switch's record onto the payment.
func (e *Engine) apply(p *Payment, r npci.Record) {
	switch r.Status {
	case npci.Success:
		e.set(p, Success, r.Code)
	case npci.Failed:
		e.set(p, Failed, r.Code)
	case npci.Reversed:
		e.set(p, Reversed, r.Code)
	case npci.Deemed:
		if p.State != Deemed {
			e.set(p, Deemed, r.Code)
		}
		if p.Checks == 0 {
			p.NextCheck = p.CreatedAt.Add(CheckOffsets[0])
		} else {
			e.scheduleNext(p)
		}
	}
}

// Reconcile corrects payments from the switch's settlement records, which
// have the final word (Consolidated Circular 30). Only final records are
// used; a transaction the switch never saw failed. Returns corrections made.
func (e *Engine) Reconcile(records []npci.Record) int {
	byTxn := make(map[string]npci.Record, len(records))
	for _, r := range records {
		byTxn[r.TxnID] = r
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := make([]string, 0, len(e.payments))
	for id := range e.payments {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	n := 0
	for _, id := range ids {
		p := e.payments[id]
		r, seen := byTxn[p.TxnID]
		want := p.State
		switch {
		case !seen:
			want = Failed
		case r.Status == npci.Success:
			want = Success
		case r.Status == npci.Failed:
			want = Failed
		case r.Status == npci.Reversed:
			want = Reversed
		default:
			continue // not final in the switch yet
		}
		if r.ReversalDue {
			continue
		}
		if want != p.State {
			e.stats.ReconCorrections[string(p.State)+"->"+string(want)]++
			e.set(p, want, "RECON")
			n++
		}
		p.ReconRequired = false
		e.persist(p)
	}
	return n
}

// Get returns a payment by ID.
func (e *Engine) Get(id string) (Payment, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.payments[id]
	if !ok {
		return Payment{}, false
	}
	return clone(p), true
}

// Payments returns all payments sorted by ID.
func (e *Engine) Payments() []Payment {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Payment, 0, len(e.payments))
	for _, p := range e.payments {
		out = append(out, clone(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Stats returns counters.
func (e *Engine) Stats() Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.stats
	s.ReconCorrections = make(map[string]int, len(e.stats.ReconCorrections))
	for k, v := range e.stats.ReconCorrections {
		s.ReconCorrections[k] = v
	}
	return s
}

func (e *Engine) set(p *Payment, s State, code string) {
	p.History = append(p.History, fmt.Sprintf("%s %s->%s %s", e.Clock.Now().Format(time.TimeOnly), p.State, s, code))
	p.State, p.Code, p.UpdatedAt = s, code, e.Clock.Now()
}

func clone(p *Payment) Payment {
	c := *p
	c.History = append([]string(nil), p.History...)
	return c
}

type result[T any] struct {
	v   T
	err error
}
