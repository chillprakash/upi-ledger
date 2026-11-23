// Package upitest builds a small two-bank world with a scripted network for
// exact tests of switch and app rules.
package upitest

import (
	"context"
	"testing"
	"time"

	"github.com/chillprakash/upi-ledger/internal/bank"
	"github.com/chillprakash/upi-ledger/internal/clock"
	"github.com/chillprakash/upi-ledger/internal/ledger"
	"github.com/chillprakash/upi-ledger/internal/money"
	"github.com/chillprakash/upi-ledger/internal/netsim"
	"github.com/chillprakash/upi-ledger/internal/npci"
	"github.com/chillprakash/upi-ledger/internal/psp"
)

// World is asha@hdfc paying ravi@icici (and frozen@icici, which refuses credits).
type World struct {
	Clock  *clock.Sim
	Script *netsim.Script
	Net    *netsim.Net
	Switch *npci.Switch
	HDFC   *bank.Bank
	ICICI  *bank.Bank
	App    *psp.Engine
	Start  time.Time
}

// New builds the world. Asha starts with Rs 1,000.
func New(t *testing.T) *World {
	t.Helper()
	ctx := context.Background()
	start := time.Date(2026, 11, 23, 10, 0, 0, 0, time.FixedZone("IST", 19800))
	w := &World{Clock: clock.NewSim(start), Script: &netsim.Script{Faults: map[string][]netsim.FaultKind{}}, Start: start}
	w.Net = &netsim.Net{Clock: w.Clock, Plan: w.Script, Latency: 200 * time.Millisecond}
	w.Switch = npci.New(w.Net, w.Clock)
	var err error
	if w.HDFC, err = bank.New(ctx, "hdfc", ledger.NewMemory()); err != nil {
		t.Fatal(err)
	}
	if w.ICICI, err = bank.New(ctx, "icici", ledger.NewMemory()); err != nil {
		t.Fatal(err)
	}
	must(t, w.HDFC.OpenAccount(ctx, "asha", money.Rupees(1000)))
	must(t, w.ICICI.OpenAccount(ctx, "ravi", 0))
	must(t, w.ICICI.OpenAccount(ctx, "frozen", 0))
	w.ICICI.Freeze("frozen")
	w.HDFC.Now, w.ICICI.Now = w.Clock.Now, w.Clock.Now
	w.Switch.AddBank(w.HDFC)
	w.Switch.AddBank(w.ICICI)
	w.Switch.Register(npci.Address{VPA: "asha@hdfc", Bank: "hdfc", Account: "asha", Name: "ASHA RAO"})
	w.Switch.Register(npci.Address{VPA: "ravi@icici", Bank: "icici", Account: "ravi", Name: "RAVI KUMAR"})
	w.Switch.Register(npci.Address{VPA: "frozen@icici", Bank: "icici", Account: "frozen", Name: "FROZEN ACCT"})
	w.App = psp.New(w.Switch, w.Net, w.Clock, "TST")
	return w
}

// Fault queues faults on a link ("psp>npci", "npci>hdfc", "npci>icici").
func (w *World) Fault(link string, k ...netsim.FaultKind) {
	w.Script.Faults[link] = append(w.Script.Faults[link], k...)
}

// Balance returns a customer's available balance.
func (w *World) Balance(t *testing.T, b *bank.Bank, acct ledger.AccountID) money.Paise {
	t.Helper()
	bal, err := b.Ledger.Balance(context.Background(), acct)
	must(t, err)
	return bal.Available()
}

// CheckLedgers asserts both banks' ledger invariants.
func (w *World) CheckLedgers(t *testing.T) {
	t.Helper()
	for _, b := range []*bank.Bank{w.HDFC, w.ICICI} {
		must(t, b.Ledger.CheckInvariants(context.Background()))
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
