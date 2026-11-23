package npci_test

import (
	"errors"
	"testing"
	"time"

	"github.com/chillprakash/upi-ledger/internal/bank"
	"github.com/chillprakash/upi-ledger/internal/ledger"
	"github.com/chillprakash/upi-ledger/internal/money"
	"github.com/chillprakash/upi-ledger/internal/npci"
	"github.com/chillprakash/upi-ledger/internal/upitest"
)

func at(h, m int) time.Time { return time.Date(2026, 11, 23, h, m, 0, 0, npci.IST) }

// Annexure E boundaries.
func TestCycleFor(t *testing.T) {
	cases := []struct {
		t      time.Time
		udir   bool
		refund bool
		want   string
	}{
		{at(0, 0), false, false, "2026-11-23/2"},
		{at(4, 59), false, false, "2026-11-23/2"},
		{at(5, 0), false, false, "2026-11-23/3"},
		{at(10, 30), false, false, "2026-11-23/5"},
		{at(20, 59), false, false, "2026-11-23/10"},
		{at(21, 0), false, false, "2026-11-24/1"}, // 21:00-00:00 ends on the next date
		{at(15, 59), true, false, "2026-11-23/DC1"},
		{at(16, 0), true, false, "2026-11-24/DC2"},
		{at(9, 0), false, true, "2026-11-23/DC1"}, // refunds are adjustments
	}
	for _, c := range cases {
		kind := npci.KindPay
		if c.refund {
			kind = npci.KindRefund
		}
		got := npci.CycleFor(npci.Record{FinalAt: c.t, ByUDIR: c.udir, Kind: kind}).ID()
		if got != c.want {
			t.Errorf("%s udir=%v refund=%v: got %s want %s", c.t.Format("15:04"), c.udir, c.refund, got, c.want)
		}
	}
}

func TestSettleNetsThroughRBI(t *testing.T) {
	w := upitest.New(t)
	w.Clock.Set(at(10, 0))
	pay(t, w, "T1", "ravi@icici", 300)
	pay(t, w, "T2", "ravi@icici", 200)
	pay(t, w, "T3", "frozen@icici", 100) // reversed: no interbank money
	rbi := &npci.RBI{Ledger: ledger.NewMemory()}
	if files, _ := w.Switch.Settle(ctx, rbi, at(10, 59)); len(files) != 0 {
		t.Fatal("settled before cycle end")
	}
	files, err := w.Switch.Settle(ctx, rbi, at(11, 0))
	if err != nil || len(files) != 1 {
		t.Fatalf("files %d err %v", len(files), err)
	}
	f := files[0]
	if f.Cycle.ID() != "2026-11-23/5" || len(f.Lines) != 2 || f.Net["hdfc"] != -money.Rupees(500) || f.Net["icici"] != money.Rupees(500) {
		t.Fatalf("got %+v", f)
	}
	if again, _ := w.Switch.Settle(ctx, rbi, at(23, 0)); len(again) != 0 {
		t.Fatal("settled twice")
	}
	// Pools are back to their opening positions; RBI holds the net.
	if p, _ := w.HDFC.Ledger.Balance(ctx, bank.Pool); p.Posted() != -money.Rupees(1000) {
		t.Fatalf("hdfc pool %s", p.Posted())
	}
	if p, _ := w.ICICI.Ledger.Balance(ctx, bank.Pool); p.Posted() != 0 {
		t.Fatalf("icici pool %s", p.Posted())
	}
	if b, _ := rbi.Ledger.Balance(ctx, "rbi:icici"); b.Posted() != money.Rupees(500) {
		t.Fatalf("rbi icici %s", b.Posted())
	}
	if err := rbi.Ledger.CheckInvariants(ctx); err != nil {
		t.Fatal(err)
	}
	w.CheckLedgers(t)
}

func TestRefunds(t *testing.T) {
	w := upitest.New(t)
	w.Switch.Register(npci.Address{VPA: "shop@icici", Bank: "icici", Account: "ravi", Name: "RAVI STORES"})
	r, err := w.Switch.Pay(ctx, npci.PayRequest{TxnID: "T1", Payer: "asha@hdfc", Payee: "shop@icici", Amount: money.Rupees(300), Merchant: true})
	if err != nil || r.Status != npci.Success {
		t.Fatal(r, err)
	}
	pay(t, w, "P2P", "ravi@icici", 10)
	if _, err := w.Switch.Refund(ctx, npci.RefundRequest{RefundID: "R0", OrigTxn: "P2P", Amount: money.Rupees(1)}); !errors.Is(err, npci.ErrRefundNotAllowed) {
		t.Fatalf("p2p refund: %v", err)
	}
	if r, err := w.Switch.Refund(ctx, npci.RefundRequest{RefundID: "R1", OrigTxn: "T1", Amount: money.Rupees(100)}); err != nil || r.Status != npci.Success {
		t.Fatal(r, err)
	}
	if _, err := w.Switch.Refund(ctx, npci.RefundRequest{RefundID: "R1", OrigTxn: "T1", Amount: money.Rupees(100)}); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if _, err := w.Switch.Refund(ctx, npci.RefundRequest{RefundID: "R2", OrigTxn: "T1", Amount: money.Rupees(201)}); !errors.Is(err, npci.ErrRefundExceeds) {
		t.Fatalf("over-refund: %v", err)
	}
	if r, _ := w.Switch.Refund(ctx, npci.RefundRequest{RefundID: "R3", OrigTxn: "T1", Amount: money.Rupees(200)}); r.Status != npci.Success {
		t.Fatal(r)
	}
	if got := w.Balance(t, w.HDFC, "asha"); got != money.Rupees(990) {
		t.Fatalf("asha %s after full refund", got)
	}
	w.CheckLedgers(t)
}
