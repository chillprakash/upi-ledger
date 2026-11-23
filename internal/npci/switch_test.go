package npci_test

import (
	"context"
	"errors"
	"testing"

	"github.com/chillprakash/upi-ledger/internal/money"
	"github.com/chillprakash/upi-ledger/internal/netsim"
	"github.com/chillprakash/upi-ledger/internal/npci"
	"github.com/chillprakash/upi-ledger/internal/upitest"
)

var ctx = context.Background()

func pay(t *testing.T, w *upitest.World, txn, payee string, rupees int64) npci.Record {
	t.Helper()
	r, err := w.Switch.Pay(ctx, npci.PayRequest{TxnID: txn, Payer: "asha@hdfc", Payee: payee, Amount: money.Rupees(rupees)})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestHappyPath(t *testing.T) {
	w := upitest.New(t)
	r := pay(t, w, "T1", "ravi@icici", 300)
	if r.Status != npci.Success || w.Balance(t, w.HDFC, "asha") != money.Rupees(700) || w.Balance(t, w.ICICI, "ravi") != money.Rupees(300) {
		t.Fatalf("got %s", r)
	}
	w.CheckLedgers(t)
}

func TestDebitDeclinedFails(t *testing.T) {
	w := upitest.New(t)
	r := pay(t, w, "T1", "ravi@icici", 5000)
	if r.Status != npci.Failed || r.Debit != npci.LegDeclined || r.ReversalDue {
		t.Fatalf("got %s", r)
	}
}

// Debit processed but its response lost: the switch must fail the payment
// and reverse the debit, leaving the payer whole.
func TestDebitTimeoutIsReversed(t *testing.T) {
	w := upitest.New(t)
	w.Fault("npci>hdfc", netsim.LostResponse)
	r := pay(t, w, "T1", "ravi@icici", 300)
	if r.Status != npci.Failed || r.Debit != npci.LegTimeout || r.Reversal != npci.LegOK {
		t.Fatalf("got %s", r)
	}
	if got := w.Balance(t, w.HDFC, "asha"); got != money.Rupees(1000) {
		t.Fatalf("asha has %s after reversal", got)
	}
	w.CheckLedgers(t)
}

// Debit never delivered: the reversal finds nothing, moves nothing, and a
// late debit for the same transaction is refused.
func TestReversalBeforeDebitBlocksLateDebit(t *testing.T) {
	w := upitest.New(t)
	w.Fault("npci>hdfc", netsim.ConnectFail)
	r := pay(t, w, "T1", "ravi@icici", 300)
	if r.Status != npci.Failed || r.Reversal != npci.LegNoop {
		t.Fatalf("got %s", r)
	}
	if late := w.HDFC.Debit(ctx, "T1", "asha", money.Rupees(300)); late.Approved {
		t.Fatal("late debit after reversal was approved")
	}
	if got := w.Balance(t, w.HDFC, "asha"); got != money.Rupees(1000) {
		t.Fatalf("asha has %s", got)
	}
	w.CheckLedgers(t)
}

// Beneficiary refuses the credit (RET): debit is reversed once.
func TestCreditDeclinedIsReturned(t *testing.T) {
	w := upitest.New(t)
	r := pay(t, w, "T1", "frozen@icici", 300)
	if r.Status != npci.Reversed || r.Reversal != npci.LegOK {
		t.Fatalf("got %s", r)
	}
	w.Switch.Resolve(ctx)
	if got := w.Balance(t, w.HDFC, "asha"); got != money.Rupees(1000) {
		t.Fatalf("asha has %s", got)
	}
	w.CheckLedgers(t)
}

// Credit applied but response lost: DEEMED, then UDIR's status check finds
// the credit and settles SUCCESS without crediting twice.
func TestDeemedResolvesToSuccess(t *testing.T) {
	w := upitest.New(t)
	w.Fault("npci>icici", netsim.LostResponse)
	if r := pay(t, w, "T1", "ravi@icici", 300); r.Status != npci.Deemed {
		t.Fatalf("got %s", r)
	}
	if left := w.Switch.Resolve(ctx); left != 0 {
		t.Fatalf("%d left", left)
	}
	r, _ := w.Switch.CheckStatus("T1")
	if r.Status != npci.Success || w.Balance(t, w.ICICI, "ravi") != money.Rupees(300) {
		t.Fatalf("got %s, ravi %s", r, w.Balance(t, w.ICICI, "ravi"))
	}
	w.CheckLedgers(t)
}

// Credit never delivered: DEEMED, then UDIR re-sends the credit.
func TestDeemedCreditResent(t *testing.T) {
	w := upitest.New(t)
	w.Fault("npci>icici", netsim.ConnectFail)
	pay(t, w, "T1", "ravi@icici", 300)
	w.Switch.Resolve(ctx)
	r, _ := w.Switch.CheckStatus("T1")
	if r.Status != npci.Success || w.Balance(t, w.ICICI, "ravi") != money.Rupees(300) {
		t.Fatalf("got %s", r)
	}
	w.CheckLedgers(t)
}

// Reversal itself times out: it stays due and UDIR retries until done.
func TestReversalRetried(t *testing.T) {
	w := upitest.New(t)
	w.Fault("npci>hdfc", netsim.OK, netsim.ConnectFail, netsim.ConnectFail)
	r := pay(t, w, "T1", "frozen@icici", 300)
	if !r.ReversalDue {
		t.Fatalf("got %s", r)
	}
	w.Switch.Resolve(ctx) // second connect failure
	w.Switch.Resolve(ctx) // succeeds
	r, _ = w.Switch.CheckStatus("T1")
	if r.ReversalDue || w.Balance(t, w.HDFC, "asha") != money.Rupees(1000) {
		t.Fatalf("got %s", r)
	}
	w.CheckLedgers(t)
}

func TestPayIsIdempotentOnTxnID(t *testing.T) {
	w := upitest.New(t)
	pay(t, w, "T1", "ravi@icici", 300)
	pay(t, w, "T1", "ravi@icici", 300)
	if got := w.Balance(t, w.HDFC, "asha"); got != money.Rupees(700) {
		t.Fatalf("asha has %s after a repeated ReqPay", got)
	}
	_, err := w.Switch.Pay(ctx, npci.PayRequest{TxnID: "T1", Payer: "asha@hdfc", Payee: "ravi@icici", Amount: money.Rupees(1)})
	if !errors.Is(err, npci.ErrMismatch) {
		t.Fatalf("got %v", err)
	}
}
