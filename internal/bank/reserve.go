package bank

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chillprakash/upi-ledger/internal/ledger"
	"github.com/chillprakash/upi-ledger/internal/money"
)

// UPI Reserve Pay (Consolidated Circular 16; formerly Single Block Multiple
// Debits): a user blocks funds in their own account for one merchant; the
// merchant debits against the block until it is exhausted, expires or is
// revoked.
//
//   - the block is a reservation in the ledger: funds stay in the user's
//     account but cannot be spent elsewhere (16.3);
//   - one active block per user per merchant (16.4);
//   - each debit is checked against the unused block amount (16.8);
//   - P2M only (16.2); every operation notifies the user (16.11).
type Block struct {
	ID       string
	Account  ledger.AccountID
	Merchant string
	Amount   money.Paise
	Used     money.Paise
	Expiry   time.Time
	Status   string // active, exhausted, revoked, expired
	pending  string // current pending transfer ID
	seq      int
}

// Reserve Pay errors.
var (
	ErrBlockExists   = errors.New("bank: an active Reserve Pay block exists for this merchant")
	ErrBlockNotFound = errors.New("bank: Reserve Pay block not found")
	ErrBlockInactive = errors.New("bank: Reserve Pay block is not active")
)

// CodeBlockExceeded is the decline when a debit exceeds the unused block.
const CodeBlockExceeded = "BLOCK_EXCEEDED"

// Notification is a message the bank sends the user (SMS in real life).
type Notification struct {
	Account ledger.AccountID
	Event   string
	Detail  string
}

// CreateBlock reserves amt in the user's account for one merchant.
func (b *Bank) CreateBlock(ctx context.Context, id string, acct ledger.AccountID, merchant string, amt money.Paise, expiry time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.blocks == nil {
		b.blocks = map[string]*Block{}
	}
	if _, ok := b.blocks[id]; ok {
		return nil // idempotent
	}
	for _, x := range b.blocks {
		if x.Account == acct && x.Merchant == merchant && x.Status == "active" {
			return ErrBlockExists
		}
	}
	blk := &Block{ID: id, Account: acct, Merchant: merchant, Amount: amt, Expiry: expiry, Status: "active"}
	if err := b.reserve(ctx, blk, amt); err != nil {
		return err
	}
	b.blocks[id] = blk
	b.notify(acct, "block_created", fmt.Sprintf("%s blocked for %s until %s", amt, merchant, expiry.Format(time.DateOnly)))
	return nil
}

func (b *Bank) reserve(ctx context.Context, blk *Block, amt money.Paise) error {
	blk.seq++
	id := fmt.Sprintf("block:%s:%d", blk.ID, blk.seq)
	if _, _, err := b.Ledger.Apply(ctx, ledger.Transfer{ID: id, Kind: ledger.Pending, Debit: blk.Account, Credit: Pool, Amount: amt, Ref: "Reserve Pay block " + blk.ID}); err != nil {
		return err
	}
	blk.pending = id
	return nil
}

// debitBlock takes amt from a block as the debit leg of txn; b.mu held.
func (b *Bank) debitBlock(ctx context.Context, txn, blockID string, amt money.Paise, now time.Time) Result {
	blk, ok := b.blocks[blockID]
	switch {
	case !ok:
		return Result{Code: CodeInvalidAccount, Reason: "no such block"}
	case blk.Status == "active" && !now.Before(blk.Expiry):
		b.endBlock(ctx, blk, "expired")
		return Result{Code: CodeBlockExceeded, Reason: "block expired"}
	case blk.Status != "active":
		return Result{Code: CodeBlockExceeded, Reason: "block " + blk.Status}
	case amt > blk.Amount-blk.Used:
		return Result{Code: CodeBlockExceeded, Reason: "debit above unused block amount"}
	}
	// Release the reservation, take the debit, reserve what is left. All
	// under the bank's lock, so nothing else can spend in between.
	if _, _, err := b.Ledger.Apply(ctx, ledger.Transfer{ID: blk.pending + ":void", Kind: ledger.Void, PendingID: blk.pending}); err != nil {
		return result(err)
	}
	key := txn + ":debit"
	if _, _, err := b.Ledger.Apply(ctx, ledger.Transfer{ID: key, Kind: ledger.Single, Debit: blk.Account, Credit: Pool, Amount: amt, Ref: key + " (Reserve Pay " + blockID + ")"}); err != nil {
		return result(err)
	}
	blk.Used += amt
	b.notify(blk.Account, "block_debit", fmt.Sprintf("%s debited by %s, %s left", amt, blk.Merchant, blk.Amount-blk.Used))
	if left := blk.Amount - blk.Used; left > 0 {
		if err := b.reserve(ctx, blk, left); err != nil {
			return result(err)
		}
	} else {
		blk.Status, blk.pending = "exhausted", ""
	}
	return Result{Approved: true}
}

// RevokeBlock ends a block at the user's request.
func (b *Bank) RevokeBlock(ctx context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	blk, ok := b.blocks[id]
	if !ok {
		return ErrBlockNotFound
	}
	if blk.Status != "active" {
		return ErrBlockInactive
	}
	return b.endBlock(ctx, blk, "revoked")
}

// ExpireBlocks ends blocks past their expiry.
func (b *Bank) ExpireBlocks(ctx context.Context, now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, blk := range b.blocks {
		if blk.Status == "active" && !now.Before(blk.Expiry) {
			if err := b.endBlock(ctx, blk, "expired"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *Bank) endBlock(ctx context.Context, blk *Block, status string) error {
	if blk.pending != "" {
		if _, _, err := b.Ledger.Apply(ctx, ledger.Transfer{ID: blk.pending + ":void", Kind: ledger.Void, PendingID: blk.pending}); err != nil {
			return err
		}
	}
	blk.Status, blk.pending = status, ""
	b.notify(blk.Account, "block_"+status, blk.ID)
	return nil
}

// GetBlock returns a copy of a block.
func (b *Bank) GetBlock(id string) (Block, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	blk, ok := b.blocks[id]
	if !ok {
		return Block{}, false
	}
	return *blk, true
}

// Notifications returns what the bank has told users.
func (b *Bank) Notifications() []Notification {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Notification(nil), b.notes...)
}

func (b *Bank) notify(acct ledger.AccountID, event, detail string) {
	b.notes = append(b.notes, Notification{acct, event, detail})
}
