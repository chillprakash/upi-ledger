package recon_test

import (
	"context"
	"testing"

	"github.com/chillprakash/upi-ledger/internal/bank"
	"github.com/chillprakash/upi-ledger/internal/ledger"
	"github.com/chillprakash/upi-ledger/internal/money"
	"github.com/chillprakash/upi-ledger/internal/npci"
	"github.com/chillprakash/upi-ledger/internal/recon"
	"github.com/chillprakash/upi-ledger/internal/upitest"
)

func TestCleanThenBreak(t *testing.T) {
	ctx := context.Background()
	w := upitest.New(t)
	w.Switch.Pay(ctx, npci.PayRequest{TxnID: "T1", Payer: "asha@hdfc", Payee: "frozen@icici", Amount: money.Rupees(100)}) //nolint:errcheck
	rep, err := recon.Run(ctx, w.Switch, nil)
	if err != nil || len(rep.Breaks) != 0 {
		t.Fatalf("%v %+v", err, rep.Breaks)
	}
	// Someone posts a credit at the beneficiary for a transaction that failed.
	w.ICICI.Ledger.Apply(ctx, ledger.Transfer{ID: "T1:credit", Kind: ledger.Single, Debit: bank.Pool, Credit: "ravi", Amount: money.Rupees(100)}) //nolint:errcheck
	rep, _ = recon.Run(ctx, w.Switch, nil)
	if rep.ByCategory()[recon.CreditOnFailed] != 1 {
		t.Fatalf("got %+v", rep.Breaks)
	}
}
