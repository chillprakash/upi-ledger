package ledger

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Memory is an in-memory Ledger with the same semantics as Postgres. The
// simulator uses it to run millions of fault schedules quickly.
type Memory struct {
	mu        sync.Mutex
	accounts  map[AccountID]AccountKind
	balances  map[AccountID]*Balance
	transfers map[string]Transfer
	resolved  map[string]string // pending ID -> post/void transfer ID
	order     []string
}

// NewMemory returns an empty in-memory ledger.
func NewMemory() *Memory {
	return &Memory{
		accounts:  map[AccountID]AccountKind{},
		balances:  map[AccountID]*Balance{},
		transfers: map[string]Transfer{},
		resolved:  map[string]string{},
	}
}

// CreateAccount implements Ledger.
func (m *Memory) CreateAccount(_ context.Context, a Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if k, ok := m.accounts[a.ID]; ok {
		if k != a.Kind {
			return ErrAccountKindChanged
		}
		return nil
	}
	m.accounts[a.ID] = a.Kind
	m.balances[a.ID] = &Balance{}
	return nil
}

// Apply implements Ledger.
func (m *Memory) Apply(_ context.Context, t Transfer) (Transfer, bool, error) {
	if err := validate(t); err != nil {
		return Transfer{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.transfers[t.ID]; ok {
		if !sameBody(t, s) {
			return Transfer{}, false, ErrIDReuse
		}
		return s, true, nil
	}
	switch t.Kind {
	case Single, Pending:
		dk, ok1 := m.accounts[t.Debit]
		_, ok2 := m.accounts[t.Credit]
		if !ok1 || !ok2 {
			return Transfer{}, false, ErrNoAccount
		}
		db, cb := m.balances[t.Debit], m.balances[t.Credit]
		if dk == Customer && db.Available() < t.Amount {
			return Transfer{}, false, ErrInsufficientFunds
		}
		if t.Kind == Single {
			db.DebitsPosted += t.Amount
			cb.CreditsPosted += t.Amount
		} else {
			db.DebitsPending += t.Amount
			cb.CreditsPending += t.Amount
		}
	case Post, Void:
		p, ok := m.transfers[t.PendingID]
		if !ok || p.Kind != Pending {
			return Transfer{}, false, ErrPendingNotFound
		}
		if _, done := m.resolved[t.PendingID]; done {
			return Transfer{}, false, ErrPendingResolved
		}
		amt := t.Amount
		if t.Kind == Post && amt == 0 {
			amt = p.Amount
		}
		if amt > p.Amount {
			return Transfer{}, false, ErrExceedsPending
		}
		db, cb := m.balances[p.Debit], m.balances[p.Credit]
		db.DebitsPending -= p.Amount
		cb.CreditsPending -= p.Amount
		if t.Kind == Post {
			db.DebitsPosted += amt
			cb.CreditsPosted += amt
		} else {
			amt = p.Amount
		}
		t.Debit, t.Credit, t.Amount = p.Debit, p.Credit, amt
		m.resolved[t.PendingID] = t.ID
	}
	m.transfers[t.ID] = t
	m.order = append(m.order, t.ID)
	return t, false, nil
}

// Get implements Ledger.
func (m *Memory) Get(_ context.Context, id string) (Transfer, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.transfers[id]
	return t, ok, nil
}

// Transfers implements Ledger.
func (m *Memory) Transfers(context.Context) ([]Transfer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Transfer, len(m.order))
	for i, id := range m.order {
		out[i] = m.transfers[id]
	}
	return out, nil
}

// Balance implements Ledger.
func (m *Memory) Balance(_ context.Context, id AccountID) (Balance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.balances[id]
	if !ok {
		return Balance{}, ErrNoAccount
	}
	return *b, nil
}

// CheckInvariants implements Ledger: recomputes every balance from the
// transfer log and compares it with the running sums.
func (m *Memory) CheckInvariants(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	re := replay(m.order, m.transfers)
	return compare(m.accounts, func(id AccountID) Balance { return *m.balances[id] }, re)
}

// replay rebuilds balances from the transfer log in application order.
func replay(order []string, transfers map[string]Transfer) map[AccountID]*Balance {
	re := map[AccountID]*Balance{}
	get := func(id AccountID) *Balance {
		if re[id] == nil {
			re[id] = &Balance{}
		}
		return re[id]
	}
	for _, id := range order {
		t := transfers[id]
		switch t.Kind {
		case Single:
			get(t.Debit).DebitsPosted += t.Amount
			get(t.Credit).CreditsPosted += t.Amount
		case Pending:
			get(t.Debit).DebitsPending += t.Amount
			get(t.Credit).CreditsPending += t.Amount
		case Post, Void:
			p := transfers[t.PendingID]
			get(p.Debit).DebitsPending -= p.Amount
			get(p.Credit).CreditsPending -= p.Amount
			if t.Kind == Post {
				get(p.Debit).DebitsPosted += t.Amount
				get(p.Credit).CreditsPosted += t.Amount
			}
		}
	}
	return re
}

func compare(accounts map[AccountID]AccountKind, have func(AccountID) Balance, want map[AccountID]*Balance) error {
	ids := make([]string, 0, len(accounts))
	for id := range accounts {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	var posted, pending int64
	for _, s := range ids {
		id := AccountID(s)
		h := have(id)
		w := Balance{}
		if want[id] != nil {
			w = *want[id]
		}
		if h != w {
			return fmt.Errorf("ledger invariant: account %s sums %+v, log says %+v", id, h, w)
		}
		if accounts[id] == Customer && h.Available() < 0 {
			return fmt.Errorf("ledger invariant: customer %s available %s < 0", id, h.Available())
		}
		posted += int64(h.Posted())
		pending += int64(h.CreditsPending - h.DebitsPending)
	}
	if posted != 0 || pending != 0 {
		return fmt.Errorf("ledger invariant: balances sum to posted %d, pending %d (want 0, 0)", posted, pending)
	}
	return nil
}
