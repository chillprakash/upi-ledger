// Package rbi applies the Reserve Bank's turnaround-time (TAT) rules for
// failed transactions (RBI circular DPSS.CO.PD No.629/02.01.014/2019-20,
// 20 Sep 2019; cited by NPCI's Consolidated Circular 31):
//
//   - UPI fund transfer (P2P): if the payer was debited and the payee not
//     credited, the debit must be reversed by T+1; after that the bank pays
//     the customer Rs 100 per day of delay.
//   - UPI merchant payment (P2M): reversal by T+5; then Rs 100 per day.
//
// T is the IST calendar date of the transaction. Compensation is paid
// without the customer having to complain.
package rbi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/chillprakash/upi-ledger/internal/bank"
	"github.com/chillprakash/upi-ledger/internal/ledger"
	"github.com/chillprakash/upi-ledger/internal/money"
	"github.com/chillprakash/upi-ledger/internal/npci"
)

// PerDay is the compensation per day of delay.
var PerDay = money.Rupees(100)

// CompensationAccount is the remitter bank's expense account that pays it.
const CompensationAccount ledger.AccountID = "compensation-expense"

// Deadline returns the last moment a reversal is on time: end of T+1 for
// P2P, end of T+5 for merchant payments, in IST.
func Deadline(r npci.Record) time.Time {
	days := 1
	if r.Merchant {
		days = 5
	}
	t := r.CreatedAt.In(npci.IST)
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, npci.IST)
	return day.AddDate(0, 0, days+1) // start of the day after the deadline day
}

// Resolved returns when the payer's money stopped being in limbo: when the
// credit finally landed (a deemed payment that succeeded), or when the debit
// was reversed. Zero if neither has happened yet.
func Resolved(r npci.Record) time.Time {
	if r.Status == npci.Success {
		return r.FinalAt
	}
	return r.ReversedAt
}

// DaysLate counts whole calendar days of delay beyond the deadline, judged
// when the money left limbo, or at now if it has not yet.
func DaysLate(r npci.Record, now time.Time) int {
	at := Resolved(r)
	if at.IsZero() {
		at = now
	}
	dl := Deadline(r)
	if at.Before(dl) {
		return 0
	}
	a := at.In(npci.IST)
	return int(time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, npci.IST).Sub(dl).Hours()/24) + 1
}

// Owed is what a transaction's payer is owed in compensation now: any
// payment whose debit moved and whose outcome (credit or reversal) came
// after the deadline, or has not come yet.
//
// Simplification: the payer's own bank credits the compensation. Under RBI's
// rules the bank at fault bears it (the beneficiary bank for an uncredited
// P2P transfer); recovering it between banks is an interbank adjustment this
// simulator does not model.
func Owed(ctx context.Context, r npci.Record, rem *bank.Bank, now time.Time) (money.Paise, error) {
	if r.Kind != npci.KindPay {
		return 0, nil
	}
	debited, err := rem.Moved(ctx, r.TxnID, "debit")
	if err != nil || !debited {
		return 0, err
	}
	return money.Paise(DaysLate(r, now)) * PerDay, nil
}

// Pay credits compensation due for a transaction, topping up what was
// already paid. Idempotent per transaction and day count: the transfer ID
// names the day, so re-running never pays the same day twice.
func Pay(ctx context.Context, r npci.Record, rem *bank.Bank, now time.Time) (money.Paise, error) {
	owed, err := Owed(ctx, r, rem, now)
	if err != nil || owed == 0 {
		return 0, err
	}
	if err := rem.Ledger.CreateAccount(ctx, ledger.Account{ID: CompensationAccount, Kind: ledger.Settlement}); err != nil {
		return 0, err
	}
	var paid money.Paise
	for d := 1; money.Paise(d)*PerDay <= owed; d++ {
		id := fmt.Sprintf("comp:%s:%d", r.TxnID, d)
		_, replayed, err := rem.Ledger.Apply(ctx, ledger.Transfer{ID: id, Kind: ledger.Single, Debit: CompensationAccount, Credit: bankAccount(r.Payer.Account), Amount: PerDay, Ref: "RBI TAT compensation " + r.TxnID})
		if err != nil {
			return paid, err
		}
		if !replayed {
			paid += PerDay
		}
	}
	return paid, nil
}

// bankAccount maps a UPI LITE account to its source bank account:
// compensation is paid into the bank account, never the LITE wallet.
func bankAccount(a ledger.AccountID) ledger.AccountID {
	return ledger.AccountID(strings.TrimPrefix(string(a), "lite:"))
}
