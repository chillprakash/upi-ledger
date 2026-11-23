// Package recon is daily automated reconciliation across the three records
// of every UPI transaction (Consolidated Circular 30): the banks' ledgers,
// the switch's records and settlement files, and the app's payment states.
// Each disagreement is a break with a category, so operations can see at a
// glance what kind of money problem exists and where.
package recon

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/chillprakash/upi-ledger/internal/npci"
	"github.com/chillprakash/upi-ledger/internal/psp"
)

// Break categories.
const (
	SettledWithoutDebit  = "settled_without_debit"
	SettledWithoutCredit = "settled_without_credit"
	SettledButReversed   = "settled_but_reversed"
	SettledTwice         = "settled_twice"
	UnsettledSuccess     = "unsettled_success"
	CreditOnFailed       = "credit_on_failed"
	UnreversedDebit      = "unreversed_debit"
	ReversalWithoutDebit = "reversal_without_debit"
	UnknownTransaction   = "ledger_entry_without_switch_record"
	AppStateMismatch     = "app_state_mismatch"
	NotFinal             = "not_final"
)

// Break is one disagreement.
type Break struct {
	Category string
	TxnID    string
	Detail   string
}

// Report is the outcome of a reconciliation run.
type Report struct {
	Transactions int
	Lines        int
	Breaks       []Break
}

// ByCategory counts breaks per category.
func (r Report) ByCategory() map[string]int {
	m := map[string]int{}
	for _, b := range r.Breaks {
		m[b.Category]++
	}
	return m
}

// Run reconciles. payments may be nil when the app side is not in scope.
func Run(ctx context.Context, sw *npci.Switch, payments []psp.Payment) (Report, error) {
	var rep Report
	add := func(cat, txn, format string, a ...any) {
		rep.Breaks = append(rep.Breaks, Break{cat, txn, fmt.Sprintf(format, a...)})
	}
	banks := sw.Banks()
	records := map[string]npci.Record{}
	for _, r := range sw.Records() {
		records[r.TxnID] = r
	}
	rep.Transactions = len(records)

	// Which legs moved money, from each bank's ledger (not its decision table).
	moved := map[string]map[string]bool{} // txn -> leg -> moved, keyed per bank via record lookup
	for name, b := range banks {
		ts, err := b.Ledger.Transfers(ctx)
		if err != nil {
			return rep, err
		}
		for _, t := range ts {
			txn, leg, ok := strings.Cut(t.ID, ":")
			if !ok || (leg != "debit" && leg != "credit" && leg != "reversal") {
				continue // openings, settlement, compensation
			}
			r, known := records[txn]
			if !known {
				add(UnknownTransaction, txn, "%s ledger has %s", name, t.ID)
				continue
			}
			// The leg must have happened at the bank the switch routed it to.
			want := r.Payer.Bank
			if leg == "credit" {
				want = r.Payee.Bank
			}
			if want != name {
				add(UnknownTransaction, txn, "%s at %s, expected %s", leg, name, want)
				continue
			}
			if moved[txn] == nil {
				moved[txn] = map[string]bool{}
			}
			moved[txn][leg] = true
		}
	}

	settledIn := map[string]string{}
	for _, f := range sw.Files() {
		for _, l := range f.Lines {
			rep.Lines++
			if prev, dup := settledIn[l.TxnID]; dup {
				add(SettledTwice, l.TxnID, "in %s and %s", prev, f.Cycle.ID())
			}
			settledIn[l.TxnID] = f.Cycle.ID()
			m := moved[l.TxnID]
			switch {
			case !m["debit"]:
				add(SettledWithoutDebit, l.TxnID, "cycle %s", f.Cycle.ID())
			case !m["credit"]:
				add(SettledWithoutCredit, l.TxnID, "cycle %s", f.Cycle.ID())
			case m["reversal"]:
				add(SettledButReversed, l.TxnID, "cycle %s", f.Cycle.ID())
			}
		}
	}

	ids := make([]string, 0, len(records))
	for id := range records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		r, m := records[id], moved[id]
		if !r.Status.Final() || r.ReversalDue {
			add(NotFinal, id, "%s", r)
			continue
		}
		if m["reversal"] && !m["debit"] {
			add(ReversalWithoutDebit, id, "")
		}
		switch r.Status {
		case npci.Success:
			if settledIn[id] == "" {
				add(UnsettledSuccess, id, "")
			}
		default:
			if m["credit"] {
				add(CreditOnFailed, id, "%s", r.Status)
			}
			if m["debit"] && !m["reversal"] {
				add(UnreversedDebit, id, "%s", r.Status)
			}
		}
	}

	want := map[npci.Status]psp.State{npci.Success: psp.Success, npci.Failed: psp.Failed, npci.Reversed: psp.Reversed}
	for _, p := range payments {
		r, ok := records[p.TxnID]
		switch {
		case !ok && p.State != psp.Failed:
			add(AppStateMismatch, p.TxnID, "app %s, switch never saw it", p.State)
		case ok && want[r.Status] != p.State:
			add(AppStateMismatch, p.TxnID, "app %s, switch %s", p.State, r.Status)
		}
	}
	return rep, nil
}
