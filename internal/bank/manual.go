package bank

import (
	"context"
	"errors"
	"time"

	"github.com/chillprakash/upi-ledger/internal/ledger"
	"github.com/chillprakash/upi-ledger/internal/money"
)

// Manual-entry errors (Consolidated Circular 32.4, 32.9).
var (
	ErrManualOnT0       = errors.New("bank: no manual credit or reversal on the transaction date (T+0) for members live on UDIR")
	ErrAlreadyResolved  = errors.New("bank: transaction already reversed or credited online")
	ErrManualNoOriginal = errors.New("bank: no debit to reverse manually")
)

// ManualReversal is an operations team's manual reversal of a debit found
// during reconciliation. Rules: never on T+0 (wait for the latest settlement
// and adjustment reports on T+1), and never when the money was already
// returned or credited online. It uses the same transfer ID as the online
// reversal, so the two can never both happen.
func (b *Bank) ManualReversal(ctx context.Context, txn string, acct ledger.AccountID, amt money.Paise, txnTime, now time.Time, ist *time.Location) error {
	if sameDate(txnTime, now, ist) {
		return ErrManualOnT0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, ok := b.decisions[txn+":reversal"]; ok && r.Approved {
		return ErrAlreadyResolved
	}
	if d, ok := b.decisions[txn+":debit"]; !ok || !d.Approved {
		return ErrManualNoOriginal
	}
	key := txn + ":reversal"
	_, replayed, err := b.Ledger.Apply(ctx, ledger.Transfer{ID: key, Kind: ledger.Single, Debit: Pool, Credit: acct, Amount: amt, Ref: key + " (manual)"})
	if err != nil {
		return err
	}
	if replayed {
		return ErrAlreadyResolved
	}
	b.decisions[key] = Result{Approved: true, Reason: "manual reversal"}
	return nil
}

func sameDate(a, b time.Time, loc *time.Location) bool {
	a, b = a.In(loc), b.In(loc)
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
}
