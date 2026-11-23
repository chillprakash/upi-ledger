package npci

import (
	"context"
	"errors"
	"strings"

	"github.com/chillprakash/upi-ledger/internal/ledger"

	"github.com/chillprakash/upi-ledger/internal/money"
)

// RefundRequest is a merchant's refund of an earlier payment, partial or full
// (Consolidated Circular 32.4, 32.10: the online refund API).
type RefundRequest struct {
	RefundID string
	OrigTxn  string
	Amount   money.Paise
}

// Refund errors.
var (
	ErrRefundNotAllowed = errors.New("npci: refund allowed only for successful merchant payments")
	ErrRefundExceeds    = errors.New("npci: refunds would exceed the original amount")
)

// Refund runs a refund as its own transaction in the reverse direction:
// debit the merchant at the beneficiary bank, credit the customer at the
// remitter bank. It reuses every rule of a payment (deadlines, deemed,
// reversal, UDIR). Idempotent on RefundID. Refunds in flight count against
// the original amount, so concurrent refunds can never over-refund.
func (s *Switch) Refund(ctx context.Context, req RefundRequest) (Record, error) {
	ctx = context.WithoutCancel(ctx)
	s.mu.Lock()
	if r, ok := s.records[req.RefundID]; ok {
		defer s.mu.Unlock()
		if r.Kind != KindRefund || r.OrigTxn != req.OrigTxn || r.Amount != req.Amount {
			return Record{}, ErrMismatch
		}
		return *r, nil
	}
	orig, ok := s.records[req.OrigTxn]
	if !ok || orig.Kind != KindPay || !orig.Merchant || orig.Status != Success || req.Amount <= 0 {
		s.mu.Unlock()
		return Record{}, ErrRefundNotAllowed
	}
	var refunded money.Paise
	for _, r := range s.records {
		if r.Kind == KindRefund && r.OrigTxn == req.OrigTxn && r.Status != Failed && r.Status != Reversed {
			refunded += r.Amount
		}
	}
	if refunded+req.Amount > orig.Amount {
		s.mu.Unlock()
		return Record{}, ErrRefundExceeds
	}
	now := s.Clock.Now()
	// The refund goes to the customer's bank account, never a UPI LITE
	// wallet (which could break its Rs 5,000 cap).
	customer := orig.Payer
	customer.Account = ledger.AccountID(strings.TrimPrefix(string(customer.Account), "lite:"))
	r := &Record{TxnID: req.RefundID, Payer: orig.Payee, Payee: customer, Amount: req.Amount,
		CreatedAt: now, UpdatedAt: now, Kind: KindRefund, OrigTxn: req.OrigTxn, Merchant: true}
	s.records[req.RefundID] = r
	s.mu.Unlock()
	s.process(ctx, r)
	s.mu.Lock()
	defer s.mu.Unlock()
	return *r, nil
}
