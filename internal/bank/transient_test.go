package bank_test

import (
	"context"
	"errors"
	"testing"

	"github.com/chillprakash/upi-ledger/internal/bank"
	"github.com/chillprakash/upi-ledger/internal/ledger"
	"github.com/chillprakash/upi-ledger/internal/money"
)

// flaky fails the next n Apply calls with an infrastructure error.
type flaky struct {
	ledger.Ledger
	n int
}

func (f *flaky) Apply(ctx context.Context, t ledger.Transfer) (ledger.Transfer, bool, error) {
	if f.n > 0 {
		f.n--
		return ledger.Transfer{}, false, errors.New("connection reset")
	}
	return f.Ledger.Apply(ctx, t)
}

// A transient failure is not the bank's answer: the retry must get a real one.
func TestTransientErrorsAreNotDecisions(t *testing.T) {
	ctx := context.Background()
	l := &flaky{Ledger: ledger.NewMemory()}
	b, _ := bank.New(ctx, "hdfc", l)
	_ = b.OpenAccount(ctx, "asha", money.Rupees(1000))
	b.Debit(ctx, "T1", "asha", money.Rupees(100))
	l.n = 1
	if r := b.Reverse(ctx, "T1", "asha", money.Rupees(100)); r.Approved || !r.Transient {
		t.Fatalf("first reversal: %+v", r)
	}
	if r := b.Reverse(ctx, "T1", "asha", money.Rupees(100)); !r.Approved {
		t.Fatalf("retry stuck on the transient failure: %+v", r)
	}
	bal, _ := b.Ledger.Balance(ctx, "asha")
	if bal.Available() != money.Rupees(1000) {
		t.Fatalf("asha %s", bal.Available())
	}
	l.n = 1
	if r := b.Credit(ctx, "T2", "asha", money.Rupees(5)); !r.Transient {
		t.Fatalf("credit: %+v", r)
	}
	if r := b.Credit(ctx, "T2", "asha", money.Rupees(5)); !r.Approved {
		t.Fatalf("credit retry: %+v", r)
	}
}
