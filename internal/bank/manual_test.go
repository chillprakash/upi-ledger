package bank_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chillprakash/upi-ledger/internal/bank"
	"github.com/chillprakash/upi-ledger/internal/ledger"
	"github.com/chillprakash/upi-ledger/internal/money"
)

func TestManualReversalRules(t *testing.T) {
	ctx := context.Background()
	ist := time.FixedZone("IST", 19800)
	b, _ := bank.New(ctx, "hdfc", ledger.NewMemory())
	_ = b.OpenAccount(ctx, "asha", money.Rupees(1000))
	txnAt := time.Date(2026, 11, 23, 10, 0, 0, 0, ist)
	b.Debit(ctx, "T1", "asha", money.Rupees(100))
	if err := b.ManualReversal(ctx, "T1", "asha", money.Rupees(100), txnAt, txnAt.Add(5*time.Hour), ist); !errors.Is(err, bank.ErrManualOnT0) {
		t.Fatalf("T+0: %v", err)
	}
	if err := b.ManualReversal(ctx, "NOPE", "asha", money.Rupees(100), txnAt, txnAt.Add(24*time.Hour), ist); !errors.Is(err, bank.ErrManualNoOriginal) {
		t.Fatalf("no original: %v", err)
	}
	if err := b.ManualReversal(ctx, "T1", "asha", money.Rupees(100), txnAt, txnAt.Add(24*time.Hour), ist); err != nil {
		t.Fatalf("T+1: %v", err)
	}
	// The online reversal arriving later must not pay again.
	b.Reverse(ctx, "T1", "asha", money.Rupees(100))
	if err := b.ManualReversal(ctx, "T1", "asha", money.Rupees(100), txnAt, txnAt.Add(48*time.Hour), ist); !errors.Is(err, bank.ErrAlreadyResolved) {
		t.Fatalf("second manual: %v", err)
	}
	bal, _ := b.Ledger.Balance(ctx, "asha")
	if bal.Available() != money.Rupees(1000) {
		t.Fatalf("asha %s", bal.Available())
	}
}

// Online reversal first, manual after: refused, no double credit.
func TestManualAfterOnlineReversal(t *testing.T) {
	ctx := context.Background()
	ist := time.FixedZone("IST", 19800)
	b, _ := bank.New(ctx, "hdfc", ledger.NewMemory())
	_ = b.OpenAccount(ctx, "asha", money.Rupees(1000))
	b.Debit(ctx, "T1", "asha", money.Rupees(100))
	b.Reverse(ctx, "T1", "asha", money.Rupees(100))
	txnAt := time.Date(2026, 11, 23, 10, 0, 0, 0, ist)
	if err := b.ManualReversal(ctx, "T1", "asha", money.Rupees(100), txnAt, txnAt.Add(30*time.Hour), ist); !errors.Is(err, bank.ErrAlreadyResolved) {
		t.Fatalf("got %v", err)
	}
}
