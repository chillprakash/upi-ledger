package npci_test

import (
	"errors"
	"testing"
	"time"

	"github.com/chillprakash/upi-ledger/internal/bank"
	"github.com/chillprakash/upi-ledger/internal/money"
	"github.com/chillprakash/upi-ledger/internal/netsim"
	"github.com/chillprakash/upi-ledger/internal/npci"
	"github.com/chillprakash/upi-ledger/internal/upitest"
)

func TestUPILite(t *testing.T) {
	w := upitest.New(t)
	now := w.Clock.Now()
	if err := w.HDFC.EnableLite(ctx, "asha", false, now); !errors.Is(err, bank.ErrLiteNeedsAFA) {
		t.Fatalf("enable without AFA: %v", err)
	}
	if err := w.HDFC.EnableLite(ctx, "asha", true, now); err != nil {
		t.Fatal(err)
	}
	if err := w.HDFC.EnableLite(ctx, "asha", true, now); !errors.Is(err, bank.ErrLiteExists) {
		t.Fatalf("second LITE account: %v", err)
	}
	if err := w.HDFC.LiteTopUp(ctx, "u1", "asha", money.Rupees(600), true, now); err != nil {
		t.Fatal(err)
	}
	if err := w.HDFC.LiteTopUp(ctx, "u2", "asha", money.Rupees(4401), true, now); !errors.Is(err, bank.ErrLiteBalanceLimit) {
		t.Fatalf("above Rs 5,000: %v", err)
	}
	litePay := func(txn string, rupees int64) npci.Record {
		r, err := w.Switch.Pay(ctx, npci.PayRequest{TxnID: txn, Payer: "asha@hdfc", Payee: "ravi@icici", Amount: money.Rupees(rupees), Lite: true})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := litePay("L1", 1001); r.Status != npci.Failed || r.Code != bank.CodeLiteLimit {
		t.Fatalf("above Rs 1,000 per payment: %s %s", r, r.Code)
	}
	if r := litePay("L2", 200); r.Status != npci.Success {
		t.Fatalf("lite payment: %s", r)
	}
	if r := litePay("L3", 100); r.Code != bank.CodeLiteSyncRequired {
		t.Fatalf("second spend before sync: %s %s", r, r.Code)
	}
	w.HDFC.LiteSync("asha")
	if r := litePay("L4", 100); r.Status != npci.Success {
		t.Fatalf("after sync: %s", r)
	}
	if got := w.Balance(t, w.HDFC, bank.LiteAccount("asha")); got != money.Rupees(300) {
		t.Fatalf("LITE balance %s", got)
	}
	if err := w.HDFC.LiteTransferOut(ctx, "o1", "asha", money.Rupees(100), now); err != nil {
		t.Fatal(err)
	}
	// Six months idle: the rest goes home and the LRN is deleted.
	n, err := w.HDFC.RefundDormantLite(ctx, now.Add(bank.LiteDormant+time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("dormant: %d %v", n, err)
	}
	if got := w.Balance(t, w.HDFC, "asha"); got != money.Rupees(700) {
		t.Fatalf("asha %s", got)
	}
	w.CheckLedgers(t)
}

func TestReservePay(t *testing.T) {
	w := upitest.New(t)
	w.Switch.Register(npci.Address{VPA: "cabs@icici", Bank: "icici", Account: "ravi", Name: "CABS LTD"})
	exp := w.Clock.Now().Add(72 * time.Hour)
	if err := w.HDFC.CreateBlock(ctx, "B1", "asha", "cabs@icici", money.Rupees(600), exp); err != nil {
		t.Fatal(err)
	}
	if err := w.HDFC.CreateBlock(ctx, "B2", "asha", "cabs@icici", money.Rupees(10), exp); !errors.Is(err, bank.ErrBlockExists) {
		t.Fatalf("second block for the same merchant: %v", err)
	}
	if got := w.Balance(t, w.HDFC, "asha"); got != money.Rupees(400) {
		t.Fatalf("blocked funds still spendable: %s", got)
	}
	debit := func(txn string, rupees int64, merchant bool) npci.Record {
		r, err := w.Switch.Pay(ctx, npci.PayRequest{TxnID: txn, Payer: "asha@hdfc", Payee: "cabs@icici", Amount: money.Rupees(rupees), Merchant: merchant, Block: "B1"})
		if err != nil && !merchant {
			return npci.Record{Code: err.Error()}
		}
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := debit("D0", 10, false); r.Code != npci.ErrBlockP2M.Error() {
		t.Fatalf("P2P block debit allowed: %s", r.Code)
	}
	if r := debit("D1", 250, true); r.Status != npci.Success {
		t.Fatal(r)
	}
	if r := debit("D2", 351, true); r.Code != bank.CodeBlockExceeded {
		t.Fatalf("above unused block: %s", r.Code)
	}
	if r := debit("D3", 350, true); r.Status != npci.Success {
		t.Fatal(r)
	}
	if b, _ := w.HDFC.GetBlock("B1"); b.Status != "exhausted" {
		t.Fatalf("block %s", b.Status)
	}
	if got := w.Balance(t, w.HDFC, "asha"); got != money.Rupees(400) {
		t.Fatalf("asha %s", got)
	}
	// Revoke releases what is left of a new block.
	_ = w.HDFC.CreateBlock(ctx, "B3", "asha", "cabs@icici", money.Rupees(300), exp)
	if err := w.HDFC.RevokeBlock(ctx, "B3"); err != nil {
		t.Fatal(err)
	}
	if got := w.Balance(t, w.HDFC, "asha"); got != money.Rupees(400) {
		t.Fatalf("after revoke %s", got)
	}
	if len(w.HDFC.Notifications()) < 5 {
		t.Fatalf("user not notified of block operations: %v", w.HDFC.Notifications())
	}
	w.CheckLedgers(t)
}

// A failed LITE payment returns to LITE, unless the user topped up meanwhile
// and the return would break the Rs 5,000 cap: then it goes to the bank account.
func TestLiteReversalRespectsCap(t *testing.T) {
	w := upitest.New(t)
	now := w.Clock.Now()
	_ = w.HDFC.EnableLite(ctx, "asha", true, now)
	_ = w.HDFC.LiteTopUp(ctx, "u1", "asha", money.Rupees(800), true, now)
	if r, _ := w.Switch.Pay(ctx, npci.PayRequest{TxnID: "L1", Payer: "asha@hdfc", Payee: "frozen@icici", Amount: money.Rupees(300), Lite: true}); r.Status != npci.Reversed {
		t.Fatalf("got %s", r)
	}
	if got := w.Balance(t, w.HDFC, bank.LiteAccount("asha")); got != money.Rupees(800) {
		t.Fatalf("normal return to LITE: %s", got)
	}
	w.HDFC.LiteSync("asha")
	_ = w.HDFC.OpenAccount(ctx, "rich", money.Rupees(10000))
	w.Switch.Register(npci.Address{VPA: "rich@hdfc", Bank: "hdfc", Account: "rich", Name: "RICH"})
	_ = w.HDFC.EnableLite(ctx, "rich", true, now)
	_ = w.HDFC.LiteTopUp(ctx, "r1", "rich", money.Rupees(4800), true, now)
	// The credit leg times out, so the payment is deemed while the user tops up.
	w.Fault("npci>icici", netsim.LostResponse)
	w.ICICI.Freeze("ravi")
	if r, _ := w.Switch.Pay(ctx, npci.PayRequest{TxnID: "L2", Payer: "rich@hdfc", Payee: "ravi@icici", Amount: money.Rupees(300), Lite: true}); r.Status != npci.Deemed {
		t.Fatalf("got %s", r)
	}
	if err := w.HDFC.LiteTopUp(ctx, "r2", "rich", money.Rupees(500), true, now); err != nil {
		t.Fatal(err)
	}
	w.Switch.Resolve(ctx) // credit refused (RET) -> reversal
	if got := w.Balance(t, w.HDFC, bank.LiteAccount("rich")); got != money.Rupees(5000) {
		t.Fatalf("LITE %s, want capped at 5000", got)
	}
	if got := w.Balance(t, w.HDFC, "rich"); got != money.Rupees(10000-4800-500+300) {
		t.Fatalf("bank account %s", got)
	}
	w.CheckLedgers(t)
}

func TestRefundOfLitePaymentGoesToBankAccount(t *testing.T) {
	w := upitest.New(t)
	now := w.Clock.Now()
	w.Switch.Register(npci.Address{VPA: "shop@icici", Bank: "icici", Account: "ravi", Name: "SHOP"})
	_ = w.HDFC.EnableLite(ctx, "asha", true, now)
	_ = w.HDFC.LiteTopUp(ctx, "u1", "asha", money.Rupees(500), true, now)
	r, _ := w.Switch.Pay(ctx, npci.PayRequest{TxnID: "L1", Payer: "asha@hdfc", Payee: "shop@icici", Amount: money.Rupees(200), Lite: true, Merchant: true})
	if r.Status != npci.Success {
		t.Fatal(r)
	}
	if rf, err := w.Switch.Refund(ctx, npci.RefundRequest{RefundID: "R1", OrigTxn: "L1", Amount: money.Rupees(200)}); err != nil || rf.Status != npci.Success {
		t.Fatal(rf, err)
	}
	if got := w.Balance(t, w.HDFC, bank.LiteAccount("asha")); got != money.Rupees(300) {
		t.Fatalf("LITE %s: refund went to the wallet", got)
	}
	if got := w.Balance(t, w.HDFC, "asha"); got != money.Rupees(700) {
		t.Fatalf("bank account %s", got)
	}
}
