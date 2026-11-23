package npci

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/chillprakash/upi-ledger/internal/bank"
	"github.com/chillprakash/upi-ledger/internal/ledger"
	"github.com/chillprakash/upi-ledger/internal/money"
)

// IST is the time zone every UPI cycle and RBI deadline is defined in.
var IST = time.FixedZone("IST", 5*3600+1800)

// Cycle is one settlement window (Consolidated Circular, Annexure E).
// Authorisation cycles 1-10 settle payments that became final in their
// window; dispute cycles DC1 and DC2 settle adjustments: anything finalised
// by a UDIR resolution pass, and refunds.
type Cycle struct {
	Date string // IST date the cycle ends on (YYYY-MM-DD)
	Name string // "1".."10", "DC1", "DC2"
	End  time.Time
}

// ID is a stable key, e.g. "2026-11-23/C5".
func (c Cycle) ID() string { return c.Date + "/" + c.Name }

var authBounds = []struct {
	name     string
	from, to int // hours
}{
	{"1", 21, 24}, {"2", 0, 5}, {"3", 5, 7}, {"4", 7, 9}, {"5", 9, 11},
	{"6", 11, 13}, {"7", 13, 15}, {"8", 15, 17}, {"9", 17, 19}, {"10", 19, 21},
}

// CycleFor returns the cycle a final transaction settles in.
func CycleFor(r Record) Cycle {
	t := r.FinalAt.In(IST)
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, IST)
	if r.ByUDIR || r.Kind == KindRefund {
		if t.Hour() < 16 {
			return Cycle{Date: day.Format(time.DateOnly), Name: "DC1", End: day.Add(16 * time.Hour)}
		}
		next := day.AddDate(0, 0, 1)
		return Cycle{Date: next.Format(time.DateOnly), Name: "DC2", End: next}
	}
	h := t.Hour()
	for _, b := range authBounds {
		if h >= b.from && h < b.to {
			end := day.Add(time.Duration(b.to) * time.Hour)
			date := day
			if b.to == 24 {
				date = day.AddDate(0, 0, 1) // 21:00-00:00 ends on the next date
			}
			return Cycle{Date: date.Format(time.DateOnly), Name: b.name, End: end}
		}
	}
	panic("unreachable")
}

// SettlementLine is one transaction in a cycle's settlement file.
type SettlementLine struct {
	TxnID       string
	Kind        Kind
	Remitter    string // bank paying out
	Beneficiary string // bank receiving
	Amount      money.Paise
}

// SettlementFile is what NPCI publishes for a cycle: the lines and each
// member's net position (positive = receives).
type SettlementFile struct {
	Cycle Cycle
	Lines []SettlementLine
	Net   map[string]money.Paise
}

// RBI holds members' settlement accounts at the Reserve Bank. Each cycle's
// multilateral net settlement batch (MNSB) debits net-debit members and
// credits net-credit members through a clearing account that must end at zero.
type RBI struct{ Ledger ledger.Ledger }

// Settle closes every cycle that has ended by now and has not been settled,
// posting the MNSB in the RBI ledger and the matching entries in each bank's
// ledger (its UPI pool against its RBI settlement account). Only SUCCESS
// transactions move money between banks; failed and reversed ones net to
// zero inside the remitter bank. Returns the files produced.
func (s *Switch) Settle(ctx context.Context, rbi *RBI, now time.Time) ([]SettlementFile, error) {
	s.mu.Lock()
	if s.settled == nil {
		s.settled = map[string]string{}
	}
	byCycle := map[string]*SettlementFile{}
	for _, r := range s.records {
		if r.Status != Success || r.FinalAt.IsZero() || s.settled[r.TxnID] != "" {
			continue
		}
		c := CycleFor(*r)
		if c.End.After(now) {
			continue
		}
		f := byCycle[c.ID()]
		if f == nil {
			f = &SettlementFile{Cycle: c, Net: map[string]money.Paise{}}
			byCycle[c.ID()] = f
		}
		f.Lines = append(f.Lines, SettlementLine{TxnID: r.TxnID, Kind: r.Kind, Remitter: r.Payer.Bank, Beneficiary: r.Payee.Bank, Amount: r.Amount})
		if r.Payer.Bank != r.Payee.Bank {
			f.Net[r.Payer.Bank] -= r.Amount
			f.Net[r.Payee.Bank] += r.Amount
		}
		s.settled[r.TxnID] = c.ID()
	}
	banks := s.banks
	s.mu.Unlock()

	ids := make([]string, 0, len(byCycle))
	for id := range byCycle {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var files []SettlementFile
	for _, id := range ids {
		f := byCycle[id]
		sort.Slice(f.Lines, func(i, j int) bool { return f.Lines[i].TxnID < f.Lines[j].TxnID })
		if err := post(ctx, rbi, banks, *f); err != nil {
			return files, err
		}
		files = append(files, *f)
		s.mu.Lock()
		s.files = append(s.files, *f)
		s.mu.Unlock()
	}
	return files, nil
}

// SettlementAccount is each bank's account for its RBI settlement balance.
const SettlementAccount ledger.AccountID = "rbi-settlement"

func post(ctx context.Context, rbi *RBI, banks map[string]*bank.Bank, f SettlementFile) error {
	clearing := ledger.AccountID("mnsb:" + f.Cycle.ID())
	if err := rbi.Ledger.CreateAccount(ctx, ledger.Account{ID: clearing, Kind: ledger.Settlement}); err != nil {
		return err
	}
	names := make([]string, 0, len(f.Net))
	for n := range f.Net {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		net := f.Net[n]
		if net == 0 {
			continue
		}
		member := ledger.AccountID("rbi:" + n)
		if err := rbi.Ledger.CreateAccount(ctx, ledger.Account{ID: member, Kind: ledger.Settlement}); err != nil {
			return err
		}
		b := banks[n]
		if err := b.Ledger.CreateAccount(ctx, ledger.Account{ID: SettlementAccount, Kind: ledger.Settlement}); err != nil {
			return err
		}
		id := fmt.Sprintf("mnsb:%s:%s", f.Cycle.ID(), n)
		rbiT := ledger.Transfer{ID: id, Kind: ledger.Single, Debit: member, Credit: clearing, Amount: -net, Ref: id}
		bankT := ledger.Transfer{ID: id, Kind: ledger.Single, Debit: bank.Pool, Credit: SettlementAccount, Amount: -net, Ref: id}
		if net > 0 {
			rbiT = ledger.Transfer{ID: id, Kind: ledger.Single, Debit: clearing, Credit: member, Amount: net, Ref: id}
			bankT = ledger.Transfer{ID: id, Kind: ledger.Single, Debit: SettlementAccount, Credit: bank.Pool, Amount: net, Ref: id}
		}
		if _, _, err := rbi.Ledger.Apply(ctx, rbiT); err != nil {
			return err
		}
		if _, _, err := b.Ledger.Apply(ctx, bankT); err != nil {
			return err
		}
	}
	bal, err := rbi.Ledger.Balance(ctx, clearing)
	if err != nil {
		return err
	}
	if bal.Posted() != 0 {
		return fmt.Errorf("npci: cycle %s clearing account not zero: %s", f.Cycle.ID(), bal.Posted())
	}
	return nil
}

// Files returns every settlement file published so far.
func (s *Switch) Files() []SettlementFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SettlementFile(nil), s.files...)
}

// Unsettled lists SUCCESS transactions not yet in any settlement file.
func (s *Switch) Unsettled() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for id, r := range s.records {
		if r.Status == Success && s.settled[id] == "" {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
