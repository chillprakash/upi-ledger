// Package chat is commerce inside a conversation, the way a business takes
// payment on WhatsApp: it sends an order_details message, the customer pays
// in the chat, the business receives a payment status webhook and replies
// with an order_status message. Message shapes follow Meta's WhatsApp
// Business Platform "Payments API — India" documentation (fetched
// 2026-11-23); only the fields this engine uses are modelled.
//
// The engine's contribution is the money side: one order can be paid at
// most once however many times the customer taps "Review and pay", because
// the order's reference_id is the payment's idempotency key.
package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/chillprakash/upi-ledger/internal/money"
	"github.com/chillprakash/upi-ledger/internal/psp"
)

// Amount is Meta's money shape: value / offset (offset 100 for INR).
type Amount struct {
	Value       int64  `json:"value"`
	Offset      int64  `json:"offset"`
	Description string `json:"description,omitempty"`
}

// Paise converts to paise; INR amounts must use offset 100.
func (a Amount) Paise() (money.Paise, error) {
	if a.Offset != 100 || a.Value < 0 {
		return 0, fmt.Errorf("chat: amount must be non-negative with offset 100, got %d/%d", a.Value, a.Offset)
	}
	return money.Paise(a.Value), nil
}

// Item is one order line.
type Item struct {
	RetailerID string  `json:"retailer_id"`
	Name       string  `json:"name"`
	Amount     Amount  `json:"amount"`
	Quantity   int     `json:"quantity"`
	SaleAmount *Amount `json:"sale_amount,omitempty"`
}

// Order is the order inside order_details.
type Order struct {
	Status     string  `json:"status"`
	Items      []Item  `json:"items"`
	Subtotal   Amount  `json:"subtotal"`
	Tax        Amount  `json:"tax"`
	Shipping   *Amount `json:"shipping,omitempty"`
	Discount   *Amount `json:"discount,omitempty"`
	Expiration *struct {
		Timestamp   string `json:"timestamp"`
		Description string `json:"description"`
	} `json:"expiration,omitempty"`
}

// OrderDetails is the interactive order_details message a business sends.
type OrderDetails struct {
	Interactive struct {
		Type string `json:"type"` // "order_details"
		Body struct {
			Text string `json:"text"`
		} `json:"body"`
		Action struct {
			Name       string `json:"name"` // "review_and_pay"
			Parameters struct {
				ReferenceID string `json:"reference_id"`
				Type        string `json:"type"` // "digital-goods" or "physical-goods"
				Currency    string `json:"currency"`
				TotalAmount Amount `json:"total_amount"`
				Order       Order  `json:"order"`
			} `json:"parameters"`
		} `json:"action"`
	} `json:"interactive"`
}

// Errors for malformed orders and checkout.
var (
	ErrInvalidOrder = errors.New("chat: invalid order_details")
	ErrOrderClosed  = errors.New("chat: order is not open for payment")
)

// Parse reads and validates an order_details message: type and action,
// INR, a reference ID, and total = subtotal + tax + shipping - discount.
func Parse(raw []byte) (OrderDetails, error) {
	var od OrderDetails
	if err := json.Unmarshal(raw, &od); err != nil {
		return od, fmt.Errorf("%w: %v", ErrInvalidOrder, err)
	}
	p := od.Interactive.Action.Parameters
	switch {
	case od.Interactive.Type != "order_details" || od.Interactive.Action.Name != "review_and_pay":
		return od, fmt.Errorf("%w: not an order_details / review_and_pay message", ErrInvalidOrder)
	case p.ReferenceID == "":
		return od, fmt.Errorf("%w: reference_id required", ErrInvalidOrder)
	case p.Currency != "INR":
		return od, fmt.Errorf("%w: currency must be INR", ErrInvalidOrder)
	case p.Order.Status != "pending":
		return od, fmt.Errorf("%w: order status must be pending", ErrInvalidOrder)
	case len(p.Order.Items) == 0:
		return od, fmt.Errorf("%w: no items", ErrInvalidOrder)
	}
	sum := func(as ...*Amount) (money.Paise, error) {
		var t money.Paise
		for _, a := range as {
			if a == nil {
				continue
			}
			v, err := a.Paise()
			if err != nil {
				return 0, err
			}
			t += v
		}
		return t, nil
	}
	plus, err := sum(&p.Order.Subtotal, &p.Order.Tax, p.Order.Shipping)
	if err != nil {
		return od, fmt.Errorf("%w: %v", ErrInvalidOrder, err)
	}
	minus, err := sum(p.Order.Discount)
	if err != nil {
		return od, fmt.Errorf("%w: %v", ErrInvalidOrder, err)
	}
	total, err := p.TotalAmount.Paise()
	if err != nil || total != plus-minus || total <= 0 {
		return od, fmt.Errorf("%w: total %d != subtotal + tax + shipping - discount (%d)", ErrInvalidOrder, p.TotalAmount.Value, plus-minus)
	}
	return od, nil
}

// Total returns the order total in paise.
func (od OrderDetails) Total() money.Paise {
	return money.Paise(od.Interactive.Action.Parameters.TotalAmount.Value)
}

// ReferenceID returns the order's reference.
func (od OrderDetails) ReferenceID() string { return od.Interactive.Action.Parameters.ReferenceID }

// Checkout pays an order from the customer's UPI ID to the business. The
// idempotency key is the business and the order reference, so tapping
// "Review and pay" twice, or a retry after a dropped connection, returns
// the same payment.
func Checkout(ctx context.Context, app *psp.Engine, od OrderDetails, customer, business string, now time.Time) (psp.Payment, error) {
	if od.Expired(now) {
		return psp.Payment{}, ErrOrderClosed
	}
	key := "order:" + business + ":" + od.ReferenceID()
	return app.Create(ctx, key, psp.Request{Payer: customer, Payee: business, Amount: od.Total(), Kind: psp.Push, Merchant: true})
}

// StatusWebhook is the payment status notification the business receives.
type StatusWebhook struct {
	Object string `json:"object"` // "whatsapp_business_account"
	Entry  []struct {
		Changes []struct {
			Value struct {
				MessagingProduct string   `json:"messaging_product"`
				Statuses         []Status `json:"statuses"`
			} `json:"value"`
			Field string `json:"field"`
		} `json:"changes"`
	} `json:"entry"`
}

// Status is one statuses[] entry of type "payment".
type Status struct {
	ID          string  `json:"id"`
	RecipientID string  `json:"recipient_id"`
	Type        string  `json:"type"`   // "payment"
	Status      string  `json:"status"` // "pending", "captured", "failed"
	Payment     Payment `json:"payment"`
	Timestamp   string  `json:"timestamp"`
}

// Payment is the payment block of a status.
type Payment struct {
	ReferenceID string      `json:"reference_id"`
	Amount      Amount      `json:"amount"`
	Currency    string      `json:"currency"`
	Transaction Transaction `json:"transaction"`
}

// Transaction is the UPI transaction behind the payment.
type Transaction struct {
	ID     string `json:"id"`
	Type   string `json:"type"`   // "upi"
	Status string `json:"status"` // "success", "failed", "pending"
	Method struct {
		Type string `json:"type"` // "upi"
	} `json:"method"`
	CreatedTimestamp int64 `json:"created_timestamp"`
	UpdatedTimestamp int64 `json:"updated_timestamp"`
	Error            *struct {
		Code   string `json:"code"`
		Reason string `json:"reason"`
	} `json:"error,omitempty"`
}

// Webhook builds the payment status webhook for a payment.
func Webhook(p psp.Payment, referenceID, customerWAID string) StatusWebhook {
	status, txn := "pending", "pending"
	switch p.State {
	case psp.Success:
		status, txn = "captured", "success"
	case psp.Failed, psp.Reversed:
		status, txn = "failed", "failed"
	}
	st := Status{ID: p.ID, RecipientID: customerWAID, Type: "payment", Status: status, Timestamp: fmt.Sprint(p.UpdatedAt.Unix())}
	st.Payment = Payment{ReferenceID: referenceID, Amount: Amount{Value: int64(p.Request.Amount), Offset: 100}, Currency: "INR"}
	st.Payment.Transaction = Transaction{ID: p.TxnID, Type: "upi", Status: txn, CreatedTimestamp: p.CreatedAt.Unix(), UpdatedTimestamp: p.UpdatedAt.Unix()}
	st.Payment.Transaction.Method.Type = "upi"
	if txn == "failed" {
		st.Payment.Transaction.Error = &struct {
			Code   string `json:"code"`
			Reason string `json:"reason"`
		}{p.Code, string(p.State)}
	}
	var w StatusWebhook
	w.Object = "whatsapp_business_account"
	w.Entry = make([]struct {
		Changes []struct {
			Value struct {
				MessagingProduct string   `json:"messaging_product"`
				Statuses         []Status `json:"statuses"`
			} `json:"value"`
			Field string `json:"field"`
		} `json:"changes"`
	}, 1)
	w.Entry[0].Changes = make([]struct {
		Value struct {
			MessagingProduct string   `json:"messaging_product"`
			Statuses         []Status `json:"statuses"`
		} `json:"value"`
		Field string `json:"field"`
	}, 1)
	w.Entry[0].Changes[0].Field = "messages"
	w.Entry[0].Changes[0].Value.MessagingProduct = "whatsapp"
	w.Entry[0].Changes[0].Value.Statuses = []Status{st}
	return w
}

// OrderStatus is the order_status message the business sends after payment.
type OrderStatus struct {
	RecipientType string `json:"recipient_type"`
	To            string `json:"to"`
	Type          string `json:"type"` // "interactive"
	Interactive   struct {
		Type string `json:"type"` // "order_status"
		Body struct {
			Text string `json:"text"`
		} `json:"body"`
		Action struct {
			Name       string `json:"name"` // "review_order"
			Parameters struct {
				ReferenceID string `json:"reference_id"`
				Order       struct {
					Status      string `json:"status"` // processing | partially_shipped | shipped | completed | canceled
					Description string `json:"description,omitempty"`
				} `json:"order"`
			} `json:"parameters"`
		} `json:"action"`
	} `json:"interactive"`
}

// StatusFor returns the order_status reply to send for a payment's state,
// or false while the payment is still uncertain (never tell a customer an
// order is processing before the money is final).
func StatusFor(p psp.Payment, referenceID, to string) (OrderStatus, bool) {
	var s OrderStatus
	switch p.State {
	case psp.Success:
		s.Interactive.Action.Parameters.Order.Status = "processing"
		s.Interactive.Body.Text = "Payment received. We are preparing your order."
	case psp.Failed, psp.Reversed:
		s.Interactive.Action.Parameters.Order.Status = "canceled"
		s.Interactive.Action.Parameters.Order.Description = "Payment did not go through; no money was taken."
		s.Interactive.Body.Text = "Your payment did not go through."
	default:
		return s, false
	}
	s.RecipientType, s.To, s.Type = "individual", to, "interactive"
	s.Interactive.Type, s.Interactive.Action.Name = "order_status", "review_order"
	s.Interactive.Action.Parameters.ReferenceID = referenceID
	return s, true
}

// Expired reports whether an order's expiration timestamp has passed.
func (od OrderDetails) Expired(now time.Time) bool {
	e := od.Interactive.Action.Parameters.Order.Expiration
	if e == nil {
		return false
	}
	var ts int64
	if _, err := fmt.Sscan(e.Timestamp, &ts); err != nil {
		return false
	}
	return now.Unix() >= ts
}
