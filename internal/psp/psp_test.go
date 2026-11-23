package psp_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chillprakash/upi-ledger/internal/money"
	"github.com/chillprakash/upi-ledger/internal/netsim"
	"github.com/chillprakash/upi-ledger/internal/psp"
	"github.com/chillprakash/upi-ledger/internal/upitest"
)

var ctx = context.Background()

func req(rupees int64) psp.Request {
	return psp.Request{Payer: "asha@hdfc", Payee: "ravi@icici", Amount: money.Rupees(rupees)}
}

func TestSuccessShowsPayeeCBSName(t *testing.T) {
	w := upitest.New(t)
	p, err := w.App.Create(ctx, "k1", req(100))
	if err != nil || p.State != psp.Success || p.PayeeName != "RAVI KUMAR" {
		t.Fatalf("got %+v, %v", p, err)
	}
}

func TestIdempotencyKey(t *testing.T) {
	w := upitest.New(t)
	a, _ := w.App.Create(ctx, "k1", req(100))
	b, _ := w.App.Create(ctx, "k1", req(100))
	if a.ID != b.ID || a.TxnID != b.TxnID {
		t.Fatalf("retry made a second payment: %s %s", a.ID, b.ID)
	}
	if got := w.Balance(t, w.HDFC, "asha"); got != money.Rupees(900) {
		t.Fatalf("asha has %s", got)
	}
	if _, err := w.App.Create(ctx, "k1", req(200)); !errors.Is(err, psp.ErrIdempotencyMismatch) {
		t.Fatalf("got %v", err)
	}
}

func TestP2PCollectRejected(t *testing.T) {
	w := upitest.New(t)
	r := req(100)
	r.Kind = psp.Collect
	if _, err := w.App.Create(ctx, "k1", r); !errors.Is(err, psp.ErrP2PCollect) {
		t.Fatalf("got %v", err)
	}
}

// Annexure C: ReqPay never reached NPCI -> failed, and no status check ever.
func TestConnectFailureFailsWithoutChecks(t *testing.T) {
	w := upitest.New(t)
	w.Fault("psp>npci", netsim.OK, netsim.ConnectFail) // validate ok, ReqPay fails
	p, _ := w.App.Create(ctx, "k1", req(100))
	if p.State != psp.Failed {
		t.Fatalf("got %s", p.State)
	}
	w.Clock.Advance(3 * time.Hour)
	w.App.Tick(ctx)
	if p, _ = w.App.Get(p.ID); p.Checks != 0 {
		t.Fatalf("made %d checks", p.Checks)
	}
}

// 12.8: first status check only after 60 s.
func TestFirstCheckAfter60s(t *testing.T) {
	w := upitest.New(t)
	w.Fault("psp>npci", netsim.OK, netsim.LostResponse)
	p, _ := w.App.Create(ctx, "k1", req(100))
	if p.State != psp.Pending {
		t.Fatalf("got %s", p.State)
	}
	w.Clock.Set(p.CreatedAt.Add(59 * time.Second))
	w.App.Tick(ctx)
	if p, _ = w.App.Get(p.ID); p.Checks != 0 {
		t.Fatalf("checked at 59 s")
	}
	w.Clock.Set(p.CreatedAt.Add(60 * time.Second))
	w.App.Tick(ctx)
	if p, _ = w.App.Get(p.ID); p.Checks != 1 || p.State != psp.Success {
		t.Fatalf("after 60 s: checks %d state %s", p.Checks, p.State)
	}
}

// 12.8: at most 3 checks; then wait for the settlement file (12.4).
func TestAtMostThreeChecks(t *testing.T) {
	w := upitest.New(t)
	w.Fault("psp>npci", netsim.OK, netsim.LostResponse, netsim.LostResponse, netsim.LostResponse, netsim.LostResponse)
	p, _ := w.App.Create(ctx, "k1", req(100))
	for i := 0; i < 20; i++ {
		w.Clock.Advance(10 * time.Minute)
		w.App.Tick(ctx)
	}
	p, _ = w.App.Get(p.ID)
	if p.Checks != 3 || p.State != psp.AwaitingSettlement {
		t.Fatalf("checks %d state %s", p.Checks, p.State)
	}
	if n := w.App.Reconcile(w.Switch.Records()); n != 1 {
		t.Fatalf("reconcile corrected %d", n)
	}
	if p, _ = w.App.Get(p.ID); p.State != psp.Success {
		t.Fatalf("after recon %s", p.State)
	}
}

// 12.5 applied literally: a connection failure on a status check stops checks
// and fails the payment, even though the money moved. Reconciliation fixes it.
func TestAnnexureCOnCheckNeedsReconciliation(t *testing.T) {
	w := upitest.New(t)
	w.Fault("psp>npci", netsim.OK, netsim.LostResponse, netsim.ConnectFail)
	p, _ := w.App.Create(ctx, "k1", req(100))
	w.Clock.Advance(61 * time.Second)
	w.App.Tick(ctx)
	p, _ = w.App.Get(p.ID)
	if p.State != psp.Failed || !p.ReconRequired {
		t.Fatalf("got %s recon=%v", p.State, p.ReconRequired)
	}
	w.App.Reconcile(w.Switch.Records())
	if p, _ = w.App.Get(p.ID); p.State != psp.Success || p.ReconRequired {
		t.Fatalf("after recon %s", p.State)
	}
}

// A deemed payment is followed up by status checks until the switch settles it.
func TestDeemedFollowedUp(t *testing.T) {
	w := upitest.New(t)
	w.Fault("npci>icici", netsim.LostResponse)
	p, _ := w.App.Create(ctx, "k1", req(100))
	if p.State != psp.Deemed {
		t.Fatalf("got %s", p.State)
	}
	w.Switch.Resolve(ctx)
	w.Clock.Advance(61 * time.Second)
	w.App.Tick(ctx)
	if p, _ = w.App.Get(p.ID); p.State != psp.Success {
		t.Fatalf("got %s", p.State)
	}
}
