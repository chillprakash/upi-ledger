// Package npci simulates the NPCI UPI switch: it resolves UPI IDs, routes a
// payment's debit leg to the remitter bank and its credit leg to the
// beneficiary bank under NPCI's deadlines, keeps the authoritative record of
// every transaction (the URCS back office), and resolves transactions left
// uncertain by timeouts (UDIR's proactive checks).
//
// Rules implemented (UPI Consolidated Circular, 18 Sep 2026):
//   - 12.1  pay legs 15 s; check status and reversal 10 s.
//   - 32.3  the switch itself triggers status and resolution for pending,
//     debit-reversal-timeout and deemed (credit-timeout) transactions.
//   - 32.8  no double credit on return (RET) cases: reversal is idempotent.
//
// Outcomes:
//
//	debit declined                 -> FAILED
//	debit timeout                  -> FAILED, reversal due (no-op if never debited)
//	debit ok, credit ok            -> SUCCESS
//	debit ok, credit declined      -> FAILED, debit reversed (RET)
//	debit ok, credit timeout       -> DEEMED, resolved later to SUCCESS or REVERSED
package npci

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/chillprakash/upi-ledger/internal/bank"
	"github.com/chillprakash/upi-ledger/internal/clock"
	"github.com/chillprakash/upi-ledger/internal/ledger"
	"github.com/chillprakash/upi-ledger/internal/money"
	"github.com/chillprakash/upi-ledger/internal/netsim"
)

// Deadlines from Consolidated Circular 12.1.
const (
	PayDeadline      = 15 * time.Second
	StatusDeadline   = 10 * time.Second
	ReversalDeadline = 10 * time.Second
)

// Status is a transaction's state in the switch.
type Status string

const (
	Success  Status = "SUCCESS"
	Failed   Status = "FAILED"
	Deemed   Status = "DEEMED"   // debited, credit unconfirmed
	Reversed Status = "REVERSED" // debited, credit failed, debit returned
)

// Final reports whether no more work is due.
func (s Status) Final() bool { return s == Success || s == Failed || s == Reversed }

// Leg is the switch's view of one leg.
type Leg string

const (
	LegNone     Leg = ""
	LegOK       Leg = "ok"
	LegDeclined Leg = "declined"
	LegTimeout  Leg = "timeout"
	LegNoop     Leg = "noop" // reversal with nothing to reverse
)

// Address is what a UPI ID resolves to.
type Address struct {
	VPA     string
	Bank    string
	Account ledger.AccountID
	Name    string // CBS name, shown to the payer before authorisation (2.13)
}

// PayRequest is ReqPay from a payer PSP.
type PayRequest struct {
	TxnID    string
	Payer    string // VPA
	Payee    string // VPA
	Amount   money.Paise
	Merchant bool   // payee is a merchant (P2M): refunds allowed, RBI T+5 applies
	Lite     bool   // pay from the payer's UPI LITE balance (no PIN)
	Block    string // Reserve Pay: debit against this block (merchant-initiated)
}

// Kind of transaction.
type Kind string

const (
	KindPay    Kind = "PAY"
	KindRefund Kind = "REFUND"
)

// Record is the switch's authoritative record of a transaction (URCS).
type Record struct {
	TxnID          string
	Payer, Payee   Address
	Amount         money.Paise
	Debit, Credit  Leg
	Reversal       Leg
	ReversalDue    bool
	Status         Status
	Code           string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ResolveAttempt int

	Kind       Kind
	Merchant   bool
	OrigTxn    string    // for refunds: the payment being refunded
	FinalAt    time.Time // when the status became final
	ReversedAt time.Time // when the debit was returned (zero if never)
	ByUDIR     bool      // final status reached in a UDIR resolution pass
	Lite       bool
	Block      string
}

// Errors returned to PSPs.
var (
	ErrUnknownVPA = errors.New("npci: UPI ID not found")
	ErrNotFound   = errors.New("npci: U48 transaction ID not present")
	ErrMismatch   = errors.New("npci: transaction ID reused with different details")
	ErrBlockP2M   = errors.New("npci: Reserve Pay debits are for merchant payments only (16.2)")
)

// Switch is the simulated NPCI switch.
type Switch struct {
	Net   *netsim.Net
	Clock clock.Clock

	mu        sync.Mutex
	resolving bool // inside a UDIR pass (guarded by mu)
	banks     map[string]*bank.Bank
	vpas      map[string]Address
	records   map[string]*Record
	settled   map[string]string // txn -> cycle ID
	files     []SettlementFile
}

// New returns an empty switch.
func New(net *netsim.Net, c clock.Clock) *Switch {
	return &Switch{Net: net, Clock: c, banks: map[string]*bank.Bank{}, vpas: map[string]Address{}, records: map[string]*Record{}}
}

// AddBank connects a member bank.
func (s *Switch) AddBank(b *bank.Bank) { s.mu.Lock(); s.banks[b.Name] = b; s.mu.Unlock() }

// Register maps a UPI ID to an account.
func (s *Switch) Register(a Address) { s.mu.Lock(); s.vpas[a.VPA] = a; s.mu.Unlock() }

// ValidateAddress resolves a UPI ID (ReqValAdd).
func (s *Switch) ValidateAddress(vpa string) (Address, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.vpas[vpa]
	if !ok {
		return Address{}, ErrUnknownVPA
	}
	return a, nil
}

// Pay processes ReqPay. A repeat of the same TxnID returns the current record
// without touching the banks again (idempotent on the PSP's transaction ID).
func (s *Switch) Pay(ctx context.Context, req PayRequest) (Record, error) {
	// Once a transaction starts, its legs must finish even if the caller
	// goes away: money movement ignores the caller's cancellation.
	ctx = context.WithoutCancel(ctx)
	s.mu.Lock()
	if r, ok := s.records[req.TxnID]; ok {
		defer s.mu.Unlock()
		if r.Kind != KindPay || r.Payer.VPA != req.Payer || r.Payee.VPA != req.Payee || r.Amount != req.Amount {
			return Record{}, ErrMismatch
		}
		return *r, nil
	}
	payer, ok1 := s.vpas[req.Payer]
	payee, ok2 := s.vpas[req.Payee]
	if !ok1 || !ok2 {
		s.mu.Unlock()
		return Record{}, ErrUnknownVPA
	}
	now := s.Clock.Now()
	if req.Lite {
		payer.Account = bank.LiteAccount(payer.Account)
	}
	if req.Block != "" && !req.Merchant {
		s.mu.Unlock()
		return Record{}, ErrBlockP2M
	}
	r := &Record{TxnID: req.TxnID, Payer: payer, Payee: payee, Amount: req.Amount, CreatedAt: now, UpdatedAt: now, Kind: KindPay, Merchant: req.Merchant, Lite: req.Lite, Block: req.Block}
	s.records[req.TxnID] = r
	s.mu.Unlock()

	s.process(ctx, r)
	s.mu.Lock()
	defer s.mu.Unlock()
	return *r, nil
}

func (s *Switch) bank(name string) *bank.Bank { s.mu.Lock(); defer s.mu.Unlock(); return s.banks[name] }

func (s *Switch) process(ctx context.Context, r *Record) {
	rem, ben := s.bank(r.Payer.Bank), s.bank(r.Payee.Bank)
	res, err := netsim.Call(s.Net, "npci>"+rem.Name, PayDeadline, func() bank.Result {
		if r.Block != "" {
			return rem.DebitBlock(ctx, r.TxnID, r.Block, r.Amount)
		}
		return rem.Debit(ctx, r.TxnID, r.Payer.Account, r.Amount)
	})
	switch {
	case err != nil || res.Transient:
		s.update(r, func() { r.Debit, r.Status, r.ReversalDue, r.Code = LegTimeout, Failed, true, "RB_DEBIT_TIMEOUT" })
		s.reverse(ctx, r)
		return
	case !res.Approved:
		s.update(r, func() { r.Debit, r.Status, r.Code = LegDeclined, Failed, res.Code })
		return
	}
	s.update(r, func() { r.Debit = LegOK })
	s.credit(ctx, r, ben)
}

// credit sends (or re-sends) the credit leg. Re-sending is safe: the bank
// answers a repeat with its recorded decision.
func (s *Switch) credit(ctx context.Context, r *Record, ben *bank.Bank) {
	res, err := netsim.Call(s.Net, "npci>"+ben.Name, PayDeadline, func() bank.Result {
		return ben.Credit(ctx, r.TxnID, r.Payee.Account, r.Amount)
	})
	switch {
	case err != nil || res.Transient:
		s.update(r, func() { r.Credit, r.Status, r.Code = LegTimeout, Deemed, "RB" })
	case res.Approved:
		s.update(r, func() { r.Credit, r.Status, r.Code = LegOK, Success, "00" })
	default:
		s.update(r, func() { r.Credit, r.Status, r.ReversalDue, r.Code = LegDeclined, Reversed, true, res.Code })
		s.reverse(ctx, r)
	}
}

// reverse returns the debit to the payer. On timeout the reversal stays due
// and Resolve retries it; the bank makes repeats harmless.
func (s *Switch) reverse(ctx context.Context, r *Record) {
	rem := s.bank(r.Payer.Bank)
	res, err := netsim.Call(s.Net, "npci>"+rem.Name, ReversalDeadline, func() bank.Result {
		return rem.Reverse(ctx, r.TxnID, r.Payer.Account, r.Amount)
	})
	if err != nil || !res.Approved {
		s.update(r, func() { r.Reversal = LegTimeout })
		return
	}
	leg := LegOK
	if res.Code == bank.CodeNoOriginal {
		leg = LegNoop
	}
	s.update(r, func() {
		r.Reversal, r.ReversalDue = leg, false
		if leg == LegOK {
			r.ReversedAt = s.Clock.Now()
		}
	})
}

// Resolve is UDIR's proactive pass (32.3): it finishes every transaction left
// uncertain. Deemed transactions get their credit status checked and the
// credit re-sent; due reversals are retried. It returns how many records
// still need work.
func (s *Switch) Resolve(ctx context.Context) int {
	ctx = context.WithoutCancel(ctx)
	s.mu.Lock()
	var work []*Record
	for _, r := range s.records {
		if r.Status == Deemed || r.ReversalDue {
			work = append(work, r)
		}
	}
	s.mu.Unlock()
	sort.Slice(work, func(i, j int) bool { return work[i].TxnID < work[j].TxnID }) // deterministic
	s.mu.Lock()
	s.resolving = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.resolving = false; s.mu.Unlock() }()
	left := 0
	for _, r := range work {
		s.update(r, func() { r.ResolveAttempt++ })
		if r.Status == Deemed {
			ben := s.bank(r.Payee.Bank)
			st, err := netsim.Call(s.Net, "npci>"+ben.Name, StatusDeadline, func() bank.LegStatus {
				return ben.Status(r.TxnID, "credit")
			})
			switch {
			case err != nil:
			case st == bank.Done:
				s.update(r, func() { r.Credit, r.Status, r.Code = LegOK, Success, "00" })
			default: // not found or declined: (re)send the credit; a decline becomes RET
				s.credit(ctx, r, ben)
			}
		}
		if r.ReversalDue {
			s.reverse(ctx, r)
		}
		if r.Status == Deemed || r.ReversalDue {
			left++
		}
	}
	return left
}

// CheckStatus answers ReqChkTxn from a PSP.
func (s *Switch) CheckStatus(txnID string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[txnID]
	if !ok {
		return Record{}, ErrNotFound
	}
	return *r, nil
}

// Records returns a copy of every record, sorted by TxnID (the settlement
// file a PSP reconciles against).
func (s *Switch) Records() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TxnID < out[j].TxnID })
	return out
}

// Banks returns the connected banks by name.
func (s *Switch) Banks() map[string]*bank.Bank {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := make(map[string]*bank.Bank, len(s.banks))
	for k, v := range s.banks {
		m[k] = v
	}
	return m
}

func (s *Switch) update(r *Record, f func()) {
	s.mu.Lock()
	wasFinal := r.Status.Final() && !r.ReversalDue
	f()
	r.UpdatedAt = s.Clock.Now()
	if !wasFinal && r.Status.Final() && !r.ReversalDue && r.FinalAt.IsZero() {
		r.FinalAt = r.UpdatedAt
		r.ByUDIR = s.resolving
	}
	s.mu.Unlock()
}

// String is for debugging.
func (r Record) String() string {
	return fmt.Sprintf("%s %s %s->%s %s debit=%s credit=%s rev=%s due=%v", r.TxnID, r.Status, r.Payer.VPA, r.Payee.VPA, r.Amount, r.Debit, r.Credit, r.Reversal, r.ReversalDue)
}
