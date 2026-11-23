package psp_test

import (
	"context"
	"testing"
	"time"

	"github.com/chillprakash/upi-ledger/internal/money"
	"github.com/chillprakash/upi-ledger/internal/pgtest"
	"github.com/chillprakash/upi-ledger/internal/psp"
	"github.com/chillprakash/upi-ledger/internal/upitest"
)

func storeSuite(t *testing.T, s psp.Store) {
	ctx := context.Background()
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	p := psp.Payment{ID: "pay_00000001", IdempotencyKey: "k1", TxnID: "TXN1", Request: psp.Request{Payer: "a@x", Payee: "b@y", Amount: money.Rupees(5), Kind: psp.Push},
		PayeeName: "B", State: psp.Initiated, CreatedAt: now, UpdatedAt: now}
	if _, ins, err := s.Insert(ctx, p); err != nil || !ins {
		t.Fatal(ins, err)
	}
	dup := p
	dup.ID, dup.TxnID = "pay_00000002", "TXN2"
	got, ins, err := s.Insert(ctx, dup)
	if err != nil || ins || got.ID != p.ID {
		t.Fatalf("duplicate key: %v %v %+v", ins, err, got)
	}
	p.State, p.Checks, p.NextCheck, p.History = psp.Pending, 1, now.Add(time.Minute), []string{"a", "b"}
	if err := s.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	all, err := s.Load(ctx)
	if err != nil || len(all) != 1 || all[0].State != psp.Pending || all[0].Checks != 1 || !all[0].NextCheck.Equal(p.NextCheck) || len(all[0].History) != 2 || all[0].Request.Amount != money.Rupees(5) {
		t.Fatalf("%v %+v", err, all)
	}
}

func TestMemStore(t *testing.T) { storeSuite(t, psp.NewMemStore()) }

func TestPGStore(t *testing.T) {
	s, err := psp.NewPGStore(context.Background(), pgtest.Pool(t))
	if err != nil {
		t.Fatal(err)
	}
	storeSuite(t, s)
}

// Crash after ReqPay was sent: the restarted engine finds the payment
// mid-flight, checks status after 60 s, and settles it without a second debit.
func TestCrashAfterSendRecovers(t *testing.T) {
	w := upitest.New(t)
	store := psp.NewMemStore()
	w.App.Store = store
	w.App.Crash = func(point string) bool { return point == "after_send" }
	if _, err := w.App.Create(ctx, "k1", req(100)); err != psp.ErrCrashed {
		t.Fatalf("got %v", err)
	}
	app := psp.New(w.Switch, w.Net, w.Clock, "TST")
	app.Store = store
	if err := app.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	p, _ := app.Create(ctx, "k1", req(100)) // the user retries
	if p.State != psp.Pending || p.Code != "RECOVERED" {
		t.Fatalf("got %s %s", p.State, p.Code)
	}
	w.Clock.Advance(61 * time.Second)
	app.Tick(ctx)
	if p, _ = app.Get(p.ID); p.State != psp.Success {
		t.Fatalf("got %s", p.State)
	}
	if got := w.Balance(t, w.HDFC, "asha"); got != money.Rupees(900) {
		t.Fatalf("asha %s", got)
	}
}

// Crash before ReqPay: the switch never saw it; status says U48, the
// payment waits for settlement, and reconciliation fails it. No money moves.
func TestCrashBeforeSendFailsCleanly(t *testing.T) {
	w := upitest.New(t)
	store := psp.NewMemStore()
	w.App.Store = store
	w.App.Crash = func(point string) bool { return point == "after_insert" }
	w.App.Create(ctx, "k1", req(100)) //nolint:errcheck
	app := psp.New(w.Switch, w.Net, w.Clock, "TST")
	app.Store = store
	_ = app.Restore(ctx)
	w.Clock.Advance(61 * time.Second)
	app.Tick(ctx)
	p := app.Payments()[0]
	if p.State != psp.AwaitingSettlement || p.Code != "U48" {
		t.Fatalf("got %s %s", p.State, p.Code)
	}
	app.Reconcile(w.Switch.Records())
	if p, _ = app.Get(p.ID); p.State != psp.Failed {
		t.Fatalf("got %s", p.State)
	}
	if got := w.Balance(t, w.HDFC, "asha"); got != money.Rupees(1000) {
		t.Fatalf("asha %s", got)
	}
}
