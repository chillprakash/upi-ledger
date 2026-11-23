// Package ledger is an append-only double-entry ledger in paise with
// two-phase transfers (reserve, then post or void), modelled on TigerBeetle's
// pending transfers. Each simulated bank keeps one. Rows are never updated
// in meaning: corrections are new transfers.
//
// Invariants (spec I1-I4):
//   - every transfer debits one account and credits another by the same amount,
//     so the sum of all balances is always zero;
//   - a transfer ID is applied at most once; a replay with the same body returns
//     the stored transfer, a replay with a different body is rejected;
//   - a pending transfer is posted or voided at most once, never for more than
//     was reserved;
//   - customer accounts never go below zero available balance.
package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/chillprakash/upi-ledger/internal/money"
)

// AccountID names an account within one ledger.
type AccountID string

// AccountKind decides whether an account may go negative.
type AccountKind int

const (
	// Customer accounts must keep available balance >= 0.
	Customer AccountKind = iota + 1
	// Settlement accounts (a bank's UPI pool, NPCI positions) may go negative.
	Settlement
	// Suspense accounts hold money whose owner is not yet known; may go negative.
	Suspense
)

func (k AccountKind) String() string {
	switch k {
	case Customer:
		return "customer"
	case Settlement:
		return "settlement"
	case Suspense:
		return "suspense"
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// Account is a ledger account.
type Account struct {
	ID   AccountID
	Kind AccountKind
}

// TransferKind is the phase a transfer represents.
type TransferKind int

const (
	// Single moves money immediately (posted).
	Single TransferKind = iota + 1
	// Pending reserves money on both accounts without posting it.
	Pending
	// Post finalises a pending transfer (all or part of it).
	Post
	// Void releases a pending transfer.
	Void
)

func (k TransferKind) String() string {
	switch k {
	case Single:
		return "single"
	case Pending:
		return "pending"
	case Post:
		return "post"
	case Void:
		return "void"
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// Transfer moves Amount from Debit to Credit. For Post and Void, PendingID
// names the pending transfer; Debit and Credit are taken from it and may be
// left empty. A Post with Amount 0 posts the full pending amount.
type Transfer struct {
	ID        string
	Kind      TransferKind
	Debit     AccountID
	Credit    AccountID
	Amount    money.Paise
	PendingID string
	Ref       string // free text: the UPI txn ID and leg, for audit
}

// Balance holds the four running sums of an account.
type Balance struct {
	DebitsPosted   money.Paise
	CreditsPosted  money.Paise
	DebitsPending  money.Paise
	CreditsPending money.Paise
}

// Posted is credits minus debits that are final.
func (b Balance) Posted() money.Paise { return b.CreditsPosted - b.DebitsPosted }

// Available is what a customer can spend: posted minus reserved debits.
func (b Balance) Available() money.Paise { return b.Posted() - b.DebitsPending }

// Errors returned by Apply.
var (
	ErrInvalid            = errors.New("ledger: invalid transfer")
	ErrNoAccount          = errors.New("ledger: account not found")
	ErrInsufficientFunds  = errors.New("ledger: insufficient funds")
	ErrIDReuse            = errors.New("ledger: transfer ID reused with a different body")
	ErrPendingNotFound    = errors.New("ledger: pending transfer not found")
	ErrPendingResolved    = errors.New("ledger: pending transfer already posted or voided")
	ErrExceedsPending     = errors.New("ledger: post amount exceeds pending amount")
	ErrAccountKindChanged = errors.New("ledger: account exists with a different kind")
)

// Ledger is implemented by the in-memory and Postgres ledgers.
type Ledger interface {
	// CreateAccount is idempotent for the same kind.
	CreateAccount(ctx context.Context, a Account) error
	// Apply applies t once. replayed is true when t.ID was already applied with
	// the same body; the stored transfer is returned and nothing changes.
	Apply(ctx context.Context, t Transfer) (stored Transfer, replayed bool, err error)
	// Get returns a transfer by ID.
	Get(ctx context.Context, id string) (Transfer, bool, error)
	// Balance returns an account's sums.
	Balance(ctx context.Context, id AccountID) (Balance, error)
	// Transfers returns every transfer in the order applied.
	Transfers(ctx context.Context) ([]Transfer, error)
	// CheckInvariants verifies that balances sum to zero and match the
	// transfers that produced them.
	CheckInvariants(ctx context.Context) error
}

// validate checks the shape of a transfer before it touches storage.
func validate(t Transfer) error {
	if t.ID == "" {
		return fmt.Errorf("%w: empty ID", ErrInvalid)
	}
	switch t.Kind {
	case Single, Pending:
		if t.Debit == "" || t.Credit == "" || t.Debit == t.Credit {
			return fmt.Errorf("%w: need two distinct accounts", ErrInvalid)
		}
		if t.Amount <= 0 {
			return fmt.Errorf("%w: amount must be positive", ErrInvalid)
		}
		if t.PendingID != "" {
			return fmt.Errorf("%w: pending ID only for post or void", ErrInvalid)
		}
	case Post, Void:
		if t.PendingID == "" {
			return fmt.Errorf("%w: post or void needs a pending ID", ErrInvalid)
		}
		if t.Amount < 0 || (t.Kind == Void && t.Amount != 0) {
			return fmt.Errorf("%w: bad amount for %s", ErrInvalid, t.Kind)
		}
	default:
		return fmt.Errorf("%w: unknown kind", ErrInvalid)
	}
	return nil
}

// sameBody reports whether a replayed transfer matches the stored one. For
// post and void the stored row has accounts and amount filled in from the
// pending transfer, so only caller-supplied fields are compared.
func sameBody(req, stored Transfer) bool {
	if req.Kind != stored.Kind || req.PendingID != stored.PendingID || req.Ref != stored.Ref {
		return false
	}
	switch req.Kind {
	case Single, Pending:
		return req.Debit == stored.Debit && req.Credit == stored.Credit && req.Amount == stored.Amount
	case Post:
		return req.Amount == 0 || req.Amount == stored.Amount
	}
	return true
}
