// Package bank simulates a bank's core banking system (CBS) as it takes part
// in UPI: it debits payers (remitter role), credits payees (beneficiary
// role), reverses debits, and answers status checks. Money lives in its
// double-entry ledger: customer accounts and one UPI settlement pool.
//
//	debit     customer  -> upi-pool   transfer ID "<txn>:debit"
//	credit    upi-pool  -> customer   transfer ID "<txn>:credit"
//	reversal  upi-pool  -> customer   transfer ID "<txn>:reversal"
//
// Every operation is idempotent by UPI transaction ID and leg. The bank
// records each decision, including declines, so a duplicate or late request
// gets the same answer and never moves money twice.
package bank

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/chillprakash/upi-ledger/internal/ledger"
	"github.com/chillprakash/upi-ledger/internal/money"
)

// Pool is the bank's UPI settlement account.
const Pool ledger.AccountID = "upi-pool"

// Result is a bank's answer to a leg.
type Result struct {
	Approved bool
	Code     string // NPCI-style response code when declined
	Reason   string
	// Transient means the bank could not decide (a database or
	// infrastructure error). It is never recorded as the bank's decision, so
	// a retry gets a real answer; the switch treats it like a timeout.
	Transient bool
}

// Response codes used by the simulator (indicative, after NPCI's error list).
const (
	CodeInsufficientFunds = "Z9"
	CodeInvalidAccount    = "ZM" // also used for a frozen beneficiary: leads to RET
	CodeNoOriginal        = "NO_ORIGINAL"
	CodeReversedFirst     = "REVERSED_BEFORE_DEBIT"
)

// LegStatus is what a status check reports for one leg.
type LegStatus int

const (
	NotFound LegStatus = iota
	Done
	Declined
)

// Bank is one simulated bank.
type Bank struct {
	Name   string
	Ledger ledger.Ledger

	mu        sync.Mutex
	decisions map[string]Result // "<txn>:<leg>" -> decision
	frozen    map[ledger.AccountID]bool
	lite      map[ledger.AccountID]*lite
	blocks    map[string]*Block
	notes     []Notification
	// Now is the bank's clock (LITE activity, block expiry); nil means time.Now.
	Now func() time.Time
}

// New creates a bank with its settlement pool.
func New(ctx context.Context, name string, l ledger.Ledger) (*Bank, error) {
	if err := l.CreateAccount(ctx, ledger.Account{ID: Pool, Kind: ledger.Settlement}); err != nil {
		return nil, err
	}
	return &Bank{Name: name, Ledger: l, decisions: map[string]Result{}, frozen: map[ledger.AccountID]bool{}}, nil
}

// OpenAccount opens a customer account funded with opening paise.
func (b *Bank) OpenAccount(ctx context.Context, id ledger.AccountID, opening money.Paise) error {
	if err := b.Ledger.CreateAccount(ctx, ledger.Account{ID: id, Kind: ledger.Customer}); err != nil {
		return err
	}
	if opening == 0 {
		return nil
	}
	_, _, err := b.Ledger.Apply(ctx, ledger.Transfer{ID: "open:" + string(id), Kind: ledger.Single, Debit: Pool, Credit: id, Amount: opening, Ref: "opening balance"})
	return err
}

// Freeze makes credits to an account fail (beneficiary return, RET).
func (b *Bank) Freeze(id ledger.AccountID) { b.mu.Lock(); b.frozen[id] = true; b.mu.Unlock() }

// Debit takes amount from a payer's account. A LITE account (lite:<acct>)
// is checked against the LITE rules first.
func (b *Bank) Debit(ctx context.Context, txn string, acct ledger.AccountID, amt money.Paise) Result {
	key := txn + ":debit"
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, ok := b.decisions[key]; ok {
		return r
	}
	if isLite(acct) {
		if r := b.liteGate(txn, acct, amt, b.now()); r != nil {
			b.decisions[key] = *r
			return *r
		}
	}
	_, _, err := b.Ledger.Apply(ctx, ledger.Transfer{ID: key, Kind: ledger.Single, Debit: acct, Credit: Pool, Amount: amt, Ref: key})
	r := result(err)
	if r.Approved && isLite(acct) {
		b.liteSpent(txn, acct, b.now())
	}
	if !r.Transient {
		b.decisions[key] = r
	}
	return r
}

// DebitBlock is the debit leg of a Reserve Pay merchant debit.
func (b *Bank) DebitBlock(ctx context.Context, txn, blockID string, amt money.Paise) Result {
	key := txn + ":debit"
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, ok := b.decisions[key]; ok {
		return r
	}
	r := b.debitBlock(ctx, txn, blockID, amt, b.now())
	if !r.Transient {
		b.decisions[key] = r
	}
	return r
}

func (b *Bank) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// Credit pays amount into a payee's account.
func (b *Bank) Credit(ctx context.Context, txn string, acct ledger.AccountID, amt money.Paise) Result {
	key := txn + ":credit"
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, ok := b.decisions[key]; ok {
		return r
	}
	var r Result
	if b.frozen[acct] {
		r = Result{Code: CodeInvalidAccount, Reason: "beneficiary account frozen"}
	} else {
		_, _, err := b.Ledger.Apply(ctx, ledger.Transfer{ID: key, Kind: ledger.Single, Debit: Pool, Credit: acct, Amount: amt, Ref: key})
		r = result(err)
	}
	if !r.Transient {
		b.decisions[key] = r
	}
	return r
}

// Reverse returns a debit to the payer. If the debit never happened, nothing
// moves, and the bank marks the debit as refused so a late debit request for
// the same transaction is declined instead of taking money for a payment that
// has already failed.
func (b *Bank) Reverse(ctx context.Context, txn string, acct ledger.AccountID, amt money.Paise) Result {
	key := txn + ":reversal"
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, ok := b.decisions[key]; ok {
		return r
	}
	debit, seen := b.decisions[txn+":debit"]
	var r Result
	switch {
	case !seen:
		b.decisions[txn+":debit"] = Result{Code: CodeReversedFirst, Reason: "reversal arrived before debit"}
		r = Result{Approved: true, Code: CodeNoOriginal, Reason: "no debit to reverse"}
	case !debit.Approved:
		r = Result{Approved: true, Code: CodeNoOriginal, Reason: "debit was declined"}
	default:
		to := b.reversalTarget(ctx, acct, amt)
		_, _, err := b.Ledger.Apply(ctx, ledger.Transfer{ID: key, Kind: ledger.Single, Debit: Pool, Credit: to, Amount: amt, Ref: key})
		r = result(err)
	}
	if !r.Transient {
		b.decisions[key] = r
	}
	return r
}

// Status reports what happened to one leg ("debit", "credit", "reversal").
func (b *Bank) Status(txn, leg string) LegStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.decisions[txn+":"+leg]
	switch {
	case !ok:
		return NotFound
	case r.Approved:
		return Done
	default:
		return Declined
	}
}

// Moved reports whether a leg actually moved money in the ledger. Used only
// by invariant checks, which must not trust the decision table.
func (b *Bank) Moved(ctx context.Context, txn, leg string) (bool, error) {
	_, ok, err := b.Ledger.Get(ctx, txn+":"+leg)
	return ok, err
}

func result(err error) Result {
	switch {
	case err == nil:
		return Result{Approved: true}
	case errors.Is(err, ledger.ErrInsufficientFunds):
		return Result{Code: CodeInsufficientFunds, Reason: "insufficient funds"}
	case errors.Is(err, ledger.ErrNoAccount):
		return Result{Code: CodeInvalidAccount, Reason: "invalid account"}
	default:
		return Result{Code: "XX", Reason: fmt.Sprintf("bank error: %v", err), Transient: true}
	}
}
