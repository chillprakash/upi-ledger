package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chillprakash/upi-ledger/internal/chat"
	"github.com/chillprakash/upi-ledger/internal/money"
	"github.com/chillprakash/upi-ledger/internal/npci"
	"github.com/chillprakash/upi-ledger/internal/psp"
	"github.com/chillprakash/upi-ledger/internal/upitest"
)

func TestOrderPaidOnceAndAnswered(t *testing.T) {
	ctx := context.Background()
	w := upitest.New(t)
	w.Switch.Register(npci.Address{VPA: "shop@icici", Bank: "icici", Account: "ravi", Name: "RAVI STORES"})
	raw, _ := os.ReadFile("testdata/order_details.json")
	od, err := chat.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if od.Total() != money.Rupees(210) {
		t.Fatalf("total %s", od.Total())
	}
	p1, err := chat.Checkout(ctx, w.App, od, "asha@hdfc", "shop@icici", w.Clock.Now())
	if err != nil || p1.State != psp.Success {
		t.Fatal(p1, err)
	}
	p2, _ := chat.Checkout(ctx, w.App, od, "asha@hdfc", "shop@icici", w.Clock.Now()) // tapped again
	if p2.ID != p1.ID {
		t.Fatal("order paid twice")
	}
	if got := w.Balance(t, w.HDFC, "asha"); got != money.Rupees(790) {
		t.Fatalf("asha %s", got)
	}
	hook, _ := json.Marshal(chat.Webhook(p1, od.ReferenceID(), "919999999999"))
	for _, want := range []string{`"type":"payment"`, `"status":"captured"`, `"reference_id":"ORD-1001"`, `"value":21000`, `"offset":100`, `"status":"success"`} {
		if !strings.Contains(string(hook), want) {
			t.Fatalf("webhook missing %s: %s", want, hook)
		}
	}
	st, ok := chat.StatusFor(p1, od.ReferenceID(), "919999999999")
	if !ok || st.Interactive.Type != "order_status" || st.Interactive.Action.Parameters.Order.Status != "processing" {
		t.Fatalf("%+v", st)
	}
	if _, ok := chat.StatusFor(psp.Payment{State: psp.Pending}, "x", "y"); ok {
		t.Fatal("order status sent while payment uncertain")
	}
}

func TestRejectsBadOrders(t *testing.T) {
	raw, _ := os.ReadFile("testdata/order_details.json")
	for name, edit := range map[string][2]string{
		"wrong total":    {`"total_amount": {"value": 21000`, `"total_amount": {"value": 20999`},
		"wrong currency": {`"currency": "INR"`, `"currency": "USD"`},
		"wrong offset":   {`"subtotal": {"value": 20000, "offset": 100}`, `"subtotal": {"value": 200, "offset": 1}`},
		"no reference":   {`"reference_id": "ORD-1001"`, `"reference_id": ""`},
	} {
		if _, err := chat.Parse([]byte(strings.Replace(string(raw), edit[0], edit[1], 1))); !errors.Is(err, chat.ErrInvalidOrder) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestExpiredOrderNotPayable(t *testing.T) {
	w := upitest.New(t)
	raw, _ := os.ReadFile("testdata/order_details.json")
	s := strings.Replace(string(raw), `"status": "pending",`, `"status": "pending", "expiration": {"timestamp": "1700000000", "description": "expired"},`, 1)
	od, err := chat.Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chat.Checkout(context.Background(), w.App, od, "asha@hdfc", "ravi@icici", time.Now()); !errors.Is(err, chat.ErrOrderClosed) {
		t.Fatalf("got %v", err)
	}
}
