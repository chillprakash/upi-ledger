package bank

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/chillprakash/upi-ledger/internal/ledger"
	"github.com/chillprakash/upi-ledger/internal/money"
)

// UPI LITE (Consolidated Circular 18; limits from RBI's framework for
// small-value offline payments, 2024): a low-value balance the remitter bank
// holds per user (its LITE Reference Number, LRN), mirrored on the device,
// spent without a UPI PIN.
//
//   - one LITE account per user per app (18.13);
//   - enable and top-up only after additional-factor authentication (18.4);
//   - transfer out to the source account at any time (18.3);
//   - each payment at most Rs 1,000, balance at most Rs 5,000;
//   - the device must sync after each payment before the next (the bank
//     declines a second unsynced spend);
//   - balances dormant for six months go back to the source account and the
//     LRN is deleted (18.10).
var (
	LitePerTxn  = money.Rupees(1000)
	LiteBalance = money.Rupees(5000)
	LiteDormant = 6 * 30 * 24 * time.Hour
)

// LITE errors.
var (
	ErrLiteExists       = errors.New("bank: UPI LITE already enabled for this user")
	ErrLiteNotEnabled   = errors.New("bank: UPI LITE not enabled")
	ErrLiteNeedsAFA     = errors.New("bank: UPI LITE enable and top-up need additional-factor authentication")
	ErrLiteBalanceLimit = errors.New("bank: UPI LITE balance limit Rs 5,000")
)

// LITE decline codes on the debit leg.
const (
	CodeLiteLimit        = "LITE_TXN_LIMIT"
	CodeLiteSyncRequired = "LITE_SYNC_REQUIRED"
)

type lite struct {
	source   ledger.AccountID
	unsynced string // txn of a spend the device has not synced yet
	last     time.Time
	closed   bool
}

// LiteAccount is the ledger account holding a user's LITE balance.
func LiteAccount(source ledger.AccountID) ledger.AccountID { return "lite:" + source }

func isLite(acct ledger.AccountID) bool { return strings.HasPrefix(string(acct), "lite:") }

// EnableLite opens the user's LITE account.
func (b *Bank) EnableLite(ctx context.Context, source ledger.AccountID, afa bool, now time.Time) error {
	if !afa {
		return ErrLiteNeedsAFA
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if l, ok := b.lites()[source]; ok && !l.closed {
		return ErrLiteExists
	}
	if err := b.Ledger.CreateAccount(ctx, ledger.Account{ID: LiteAccount(source), Kind: ledger.Customer}); err != nil {
		return err
	}
	b.lites()[source] = &lite{source: source, last: now}
	return nil
}

// LiteTopUp moves money from the source account into LITE.
func (b *Bank) LiteTopUp(ctx context.Context, id string, source ledger.AccountID, amt money.Paise, afa bool, now time.Time) error {
	if !afa {
		return ErrLiteNeedsAFA
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	l, ok := b.lites()[source]
	if !ok || l.closed {
		return ErrLiteNotEnabled
	}
	bal, err := b.Ledger.Balance(ctx, LiteAccount(source))
	if err != nil {
		return err
	}
	if bal.Posted()+amt > LiteBalance {
		return ErrLiteBalanceLimit
	}
	if _, _, err := b.Ledger.Apply(ctx, ledger.Transfer{ID: "lite-topup:" + id, Kind: ledger.Single, Debit: source, Credit: LiteAccount(source), Amount: amt, Ref: "LITE top-up"}); err != nil {
		return err
	}
	l.last = now
	return nil
}

// LiteTransferOut moves LITE money back to the source account.
func (b *Bank) LiteTransferOut(ctx context.Context, id string, source ledger.AccountID, amt money.Paise, now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, ok := b.lites()[source]
	if !ok || l.closed {
		return ErrLiteNotEnabled
	}
	if _, _, err := b.Ledger.Apply(ctx, ledger.Transfer{ID: "lite-out:" + id, Kind: ledger.Single, Debit: LiteAccount(source), Credit: source, Amount: amt, Ref: "LITE transfer out"}); err != nil {
		return err
	}
	l.last = now
	return nil
}

// LiteSync records that the device has applied the bank's latest signed state.
func (b *Bank) LiteSync(source ledger.AccountID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if l, ok := b.lites()[source]; ok {
		l.unsynced = ""
	}
}

// RefundDormantLite returns balances of LITE accounts idle for six months to
// their source accounts and deletes the LRN. Returns how many were closed.
func (b *Bank) RefundDormantLite(ctx context.Context, now time.Time) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for src, l := range b.lites() {
		if l.closed || now.Sub(l.last) < LiteDormant {
			continue
		}
		bal, err := b.Ledger.Balance(ctx, LiteAccount(src))
		if err != nil {
			return n, err
		}
		if bal.Posted() > 0 {
			if _, _, err := b.Ledger.Apply(ctx, ledger.Transfer{ID: "lite-dormant:" + string(src) + ":" + now.Format("20060102"), Kind: ledger.Single, Debit: LiteAccount(src), Credit: src, Amount: bal.Posted(), Ref: "dormant LITE refund (18.10)"}); err != nil {
				return n, err
			}
		}
		l.closed = true
		n++
	}
	return n, nil
}

// liteGate checks a debit from a LITE account; called with b.mu held.
func (b *Bank) liteGate(txn string, acct ledger.AccountID, amt money.Paise, now time.Time) *Result {
	src := ledger.AccountID(strings.TrimPrefix(string(acct), "lite:"))
	l, ok := b.lites()[src]
	switch {
	case !ok || l.closed:
		return &Result{Code: CodeInvalidAccount, Reason: "LITE not enabled"}
	case amt > LitePerTxn:
		return &Result{Code: CodeLiteLimit, Reason: "UPI LITE payment above Rs 1,000"}
	case l.unsynced != "":
		return &Result{Code: CodeLiteSyncRequired, Reason: "device must sync before the next LITE payment"}
	}
	return nil
}

// liteSpent marks a successful LITE spend: the device must sync next; b.mu held.
func (b *Bank) liteSpent(txn string, acct ledger.AccountID, now time.Time) {
	if l, ok := b.lites()[ledger.AccountID(strings.TrimPrefix(string(acct), "lite:"))]; ok {
		l.unsynced, l.last = txn, now
	}
}

func (b *Bank) lites() map[ledger.AccountID]*lite {
	if b.lite == nil {
		b.lite = map[ledger.AccountID]*lite{}
	}
	return b.lite
}

// reversalTarget picks where a reversed debit goes. A failed LITE payment
// goes back to the LITE balance, unless that would take it past the
// Rs 5,000 cap (the user may have topped up meanwhile); then it goes to the
// user's bank account. Called with b.mu held.
func (b *Bank) reversalTarget(ctx context.Context, acct ledger.AccountID, amt money.Paise) ledger.AccountID {
	if !isLite(acct) {
		return acct
	}
	bal, err := b.Ledger.Balance(ctx, acct)
	if err != nil || bal.Posted()+amt > LiteBalance {
		return ledger.AccountID(strings.TrimPrefix(string(acct), "lite:"))
	}
	return acct
}
