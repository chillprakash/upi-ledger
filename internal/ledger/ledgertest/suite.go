// Package ledgertest is one behaviour suite that every Ledger implementation
// must pass, so the in-memory ledger the simulator uses and the Postgres
// ledger cannot drift apart.
package ledgertest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/chillprakash/upi-ledger/internal/ledger"
	"github.com/chillprakash/upi-ledger/internal/money"
)

// Run runs the suite. newLedger must return an empty ledger each call.
func Run(t *testing.T, newLedger func(t *testing.T) ledger.Ledger) {
	ctx := context.Background()
	setup := func(t *testing.T) ledger.Ledger {
		l := newLedger(t)
		must(t, l.CreateAccount(ctx, ledger.Account{ID: "pool", Kind: ledger.Settlement}))
		must(t, l.CreateAccount(ctx, ledger.Account{ID: "asha", Kind: ledger.Customer}))
		must(t, l.CreateAccount(ctx, ledger.Account{ID: "ravi", Kind: ledger.Customer}))
		apply(t, l, ledger.Transfer{ID: "fund-asha", Kind: ledger.Single, Debit: "pool", Credit: "asha", Amount: money.Rupees(1000)})
		return l
	}
	check := func(t *testing.T, l ledger.Ledger) {
		t.Helper()
		if err := l.CheckInvariants(ctx); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("single transfer moves money and sums to zero", func(t *testing.T) {
		l := setup(t)
		apply(t, l, ledger.Transfer{ID: "t1", Kind: ledger.Single, Debit: "asha", Credit: "ravi", Amount: money.Rupees(250)})
		wantAvail(t, l, "asha", money.Rupees(750))
		wantAvail(t, l, "ravi", money.Rupees(250))
		check(t, l)
	})

	t.Run("customer cannot go negative", func(t *testing.T) {
		l := setup(t)
		_, _, err := l.Apply(ctx, ledger.Transfer{ID: "t1", Kind: ledger.Single, Debit: "asha", Credit: "ravi", Amount: money.Rupees(1001)})
		if !errors.Is(err, ledger.ErrInsufficientFunds) {
			t.Fatalf("got %v, want insufficient funds", err)
		}
		wantAvail(t, l, "asha", money.Rupees(1000))
		check(t, l)
	})

	t.Run("replay returns the stored transfer and changes nothing", func(t *testing.T) {
		l := setup(t)
		tr := ledger.Transfer{ID: "t1", Kind: ledger.Single, Debit: "asha", Credit: "ravi", Amount: money.Rupees(100), Ref: "UPI1:debit"}
		apply(t, l, tr)
		_, replayed, err := l.Apply(ctx, tr)
		if err != nil || !replayed {
			t.Fatalf("replay: replayed=%v err=%v", replayed, err)
		}
		wantAvail(t, l, "asha", money.Rupees(900))
		check(t, l)
	})

	t.Run("same ID with a different body is rejected", func(t *testing.T) {
		l := setup(t)
		apply(t, l, ledger.Transfer{ID: "t1", Kind: ledger.Single, Debit: "asha", Credit: "ravi", Amount: money.Rupees(100)})
		_, _, err := l.Apply(ctx, ledger.Transfer{ID: "t1", Kind: ledger.Single, Debit: "asha", Credit: "ravi", Amount: money.Rupees(200)})
		if !errors.Is(err, ledger.ErrIDReuse) {
			t.Fatalf("got %v, want ID reuse", err)
		}
		check(t, l)
	})

	t.Run("pending reserves funds; post finalises once", func(t *testing.T) {
		l := setup(t)
		apply(t, l, ledger.Transfer{ID: "p1", Kind: ledger.Pending, Debit: "asha", Credit: "pool", Amount: money.Rupees(600)})
		wantAvail(t, l, "asha", money.Rupees(400))
		_, _, err := l.Apply(ctx, ledger.Transfer{ID: "p2", Kind: ledger.Pending, Debit: "asha", Credit: "pool", Amount: money.Rupees(500)})
		if !errors.Is(err, ledger.ErrInsufficientFunds) {
			t.Fatalf("second reserve: got %v, want insufficient funds", err)
		}
		apply(t, l, ledger.Transfer{ID: "p1-post", Kind: ledger.Post, PendingID: "p1"})
		wantAvail(t, l, "asha", money.Rupees(400))
		_, _, err = l.Apply(ctx, ledger.Transfer{ID: "p1-void", Kind: ledger.Void, PendingID: "p1"})
		if !errors.Is(err, ledger.ErrPendingResolved) {
			t.Fatalf("void after post: got %v, want already resolved", err)
		}
		check(t, l)
	})

	t.Run("void releases the reservation", func(t *testing.T) {
		l := setup(t)
		apply(t, l, ledger.Transfer{ID: "p1", Kind: ledger.Pending, Debit: "asha", Credit: "pool", Amount: money.Rupees(600)})
		apply(t, l, ledger.Transfer{ID: "p1-void", Kind: ledger.Void, PendingID: "p1"})
		wantAvail(t, l, "asha", money.Rupees(1000))
		check(t, l)
	})

	t.Run("partial post releases the remainder", func(t *testing.T) {
		l := setup(t)
		apply(t, l, ledger.Transfer{ID: "p1", Kind: ledger.Pending, Debit: "asha", Credit: "pool", Amount: money.Rupees(600)})
		apply(t, l, ledger.Transfer{ID: "p1-post", Kind: ledger.Post, PendingID: "p1", Amount: money.Rupees(150)})
		wantAvail(t, l, "asha", money.Rupees(850))
		_, _, err := l.Apply(ctx, ledger.Transfer{ID: "p2-post", Kind: ledger.Post, PendingID: "p1", Amount: money.Rupees(1)})
		if !errors.Is(err, ledger.ErrPendingResolved) {
			t.Fatalf("second post: got %v", err)
		}
		check(t, l)
	})

	t.Run("post cannot exceed the reservation", func(t *testing.T) {
		l := setup(t)
		apply(t, l, ledger.Transfer{ID: "p1", Kind: ledger.Pending, Debit: "asha", Credit: "pool", Amount: money.Rupees(100)})
		_, _, err := l.Apply(ctx, ledger.Transfer{ID: "p1-post", Kind: ledger.Post, PendingID: "p1", Amount: money.Rupees(101)})
		if !errors.Is(err, ledger.ErrExceedsPending) {
			t.Fatalf("got %v, want exceeds pending", err)
		}
		check(t, l)
	})

	t.Run("invalid transfers are rejected", func(t *testing.T) {
		l := setup(t)
		for _, tr := range []ledger.Transfer{
			{ID: "", Kind: ledger.Single, Debit: "asha", Credit: "ravi", Amount: 1},
			{ID: "x", Kind: ledger.Single, Debit: "asha", Credit: "asha", Amount: 1},
			{ID: "x", Kind: ledger.Single, Debit: "asha", Credit: "ravi", Amount: 0},
			{ID: "x", Kind: ledger.Post},
		} {
			if _, _, err := l.Apply(ctx, tr); !errors.Is(err, ledger.ErrInvalid) {
				t.Fatalf("%+v: got %v, want invalid", tr, err)
			}
		}
		if _, _, err := l.Apply(ctx, ledger.Transfer{ID: "x", Kind: ledger.Single, Debit: "asha", Credit: "nobody", Amount: 1}); !errors.Is(err, ledger.ErrNoAccount) {
			t.Fatalf("unknown account: got %v", err)
		}
		check(t, l)
	})

	t.Run("concurrent duplicates apply exactly once", func(t *testing.T) {
		l := setup(t)
		const workers = 16
		var wg sync.WaitGroup
		var applied, replays atomic.Int64
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 20; i++ {
					_, rep, err := l.Apply(ctx, ledger.Transfer{ID: fmt.Sprintf("dup-%d", i), Kind: ledger.Single, Debit: "asha", Credit: "ravi", Amount: money.Rupees(10)})
					if err != nil {
						t.Error(err)
						return
					}
					if rep {
						replays.Add(1)
					} else {
						applied.Add(1)
					}
				}
			}()
		}
		wg.Wait()
		if applied.Load() != 20 || replays.Load() != 20*(workers-1) {
			t.Fatalf("applied %d, replays %d", applied.Load(), replays.Load())
		}
		wantAvail(t, l, "asha", money.Rupees(800))
		check(t, l)
	})

	t.Run("concurrent spends never overdraw", func(t *testing.T) {
		l := setup(t)
		var wg sync.WaitGroup
		var ok atomic.Int64
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, _, err := l.Apply(ctx, ledger.Transfer{ID: fmt.Sprintf("s-%d", i), Kind: ledger.Single, Debit: "asha", Credit: "ravi", Amount: money.Rupees(30)})
				if err == nil {
					ok.Add(1)
				} else if !errors.Is(err, ledger.ErrInsufficientFunds) {
					t.Error(err)
				}
			}(i)
		}
		wg.Wait()
		if ok.Load() != 33 {
			t.Fatalf("%d spends of 30 succeeded from 1000; want 33", ok.Load())
		}
		wantAvail(t, l, "asha", money.Rupees(10))
		check(t, l)
	})

	t.Run("concurrent post and void resolve once", func(t *testing.T) {
		l := setup(t)
		apply(t, l, ledger.Transfer{ID: "p1", Kind: ledger.Pending, Debit: "asha", Credit: "pool", Amount: money.Rupees(100)})
		var wg sync.WaitGroup
		var wins atomic.Int64
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				kind := ledger.Post
				if i%2 == 1 {
					kind = ledger.Void
				}
				_, _, err := l.Apply(ctx, ledger.Transfer{ID: fmt.Sprintf("r-%d", i), Kind: kind, PendingID: "p1"})
				if err == nil {
					wins.Add(1)
				} else if !errors.Is(err, ledger.ErrPendingResolved) {
					t.Error(err)
				}
			}(i)
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("%d resolutions succeeded; want 1", wins.Load())
		}
		check(t, l)
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func apply(t *testing.T, l ledger.Ledger, tr ledger.Transfer) {
	t.Helper()
	if _, _, err := l.Apply(context.Background(), tr); err != nil {
		t.Fatalf("apply %s: %v", tr.ID, err)
	}
}

func wantAvail(t *testing.T, l ledger.Ledger, id ledger.AccountID, want money.Paise) {
	t.Helper()
	b, err := l.Balance(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if b.Available() != want {
		t.Fatalf("%s available %s, want %s", id, b.Available(), want)
	}
}
