package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chillprakash/upi-ledger/internal/money"
)

// Schema creates the ledger tables. Idempotency and resolve-once are enforced
// by the database: the transfers primary key, and a unique index on the
// pending ID of post/void rows.
const Schema = `
CREATE TABLE IF NOT EXISTS ledger_accounts (
	id              text PRIMARY KEY,
	kind            smallint NOT NULL,
	debits_posted   bigint NOT NULL DEFAULT 0,
	credits_posted  bigint NOT NULL DEFAULT 0,
	debits_pending  bigint NOT NULL DEFAULT 0,
	credits_pending bigint NOT NULL DEFAULT 0,
	CONSTRAINT customer_not_negative CHECK (
		kind <> 1 OR credits_posted - debits_posted - debits_pending >= 0)
);
CREATE TABLE IF NOT EXISTS ledger_transfers (
	seq        bigserial UNIQUE,
	id         text PRIMARY KEY,
	kind       smallint NOT NULL,
	debit      text NOT NULL REFERENCES ledger_accounts(id),
	credit     text NOT NULL REFERENCES ledger_accounts(id),
	amount     bigint NOT NULL CHECK (amount >= 0),
	pending_id text REFERENCES ledger_transfers(id),
	ref        text NOT NULL DEFAULT '',
	created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS ledger_pending_resolved_once
	ON ledger_transfers(pending_id) WHERE kind IN (3, 4);
`

// Postgres is a Ledger stored in Postgres.
type Postgres struct{ pool *pgxpool.Pool }

// NewPostgres creates the schema if needed and returns the ledger.
func NewPostgres(ctx context.Context, pool *pgxpool.Pool) (*Postgres, error) {
	if _, err := pool.Exec(ctx, Schema); err != nil {
		return nil, fmt.Errorf("ledger schema: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

// CreateAccount implements Ledger.
func (p *Postgres) CreateAccount(ctx context.Context, a Account) error {
	var kind int
	err := p.pool.QueryRow(ctx, `
		INSERT INTO ledger_accounts (id, kind) VALUES ($1, $2)
		ON CONFLICT (id) DO UPDATE SET id = EXCLUDED.id
		RETURNING kind`, string(a.ID), int(a.Kind)).Scan(&kind)
	if err != nil {
		return err
	}
	if AccountKind(kind) != a.Kind {
		return ErrAccountKindChanged
	}
	return nil
}

// Apply implements Ledger. One database transaction: insert the transfer row
// first (a concurrent duplicate blocks on the primary key, then sees the
// committed row and becomes a replay), lock both accounts in ID order, check
// funds, update sums.
func (p *Postgres) Apply(ctx context.Context, t Transfer) (Transfer, bool, error) {
	if err := validate(t); err != nil {
		return Transfer{}, false, err
	}
	for {
		stored, replayed, err := p.apply(ctx, t)
		if errors.Is(err, errRetry) {
			continue
		}
		return stored, replayed, err
	}
}

var errRetry = errors.New("retry")

func (p *Postgres) apply(ctx context.Context, t Transfer) (Transfer, bool, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Transfer{}, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Resolve accounts and amount for post/void from the pending transfer.
	var pend Transfer
	if t.Kind == Post || t.Kind == Void {
		var ok bool
		pend, ok, err = getTx(ctx, tx, t.PendingID, true)
		if err != nil {
			return Transfer{}, false, err
		}
		if !ok || pend.Kind != Pending {
			// It may be a replay of a post whose pending ID is wrong; check ID first.
			if s, found, _ := getTx(ctx, tx, t.ID, false); found {
				return replayOrReuse(t, s)
			}
			return Transfer{}, false, ErrPendingNotFound
		}
		t.Debit, t.Credit = pend.Debit, pend.Credit
		switch {
		case t.Kind == Void:
			t.Amount = pend.Amount
		case t.Amount == 0:
			t.Amount = pend.Amount
		case t.Amount > pend.Amount:
			return Transfer{}, false, ErrExceedsPending
		}
	}

	// Lock both accounts in a fixed order before inserting the transfer. FOR NO
	// KEY UPDATE does not conflict with the KEY SHARE lock the foreign-key
	// check takes, so concurrent spends queue here instead of deadlocking.
	a, b := t.Debit, t.Credit
	if b < a {
		a, b = b, a
	}
	rows, err := tx.Query(ctx, `SELECT id FROM ledger_accounts WHERE id IN ($1, $2) ORDER BY id FOR NO KEY UPDATE`, string(a), string(b))
	if err != nil {
		return Transfer{}, false, err
	}
	n := 0
	for rows.Next() {
		n++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Transfer{}, false, err
	}
	if n != 2 {
		return Transfer{}, false, ErrNoAccount
	}

	var pendingID any
	if t.PendingID != "" {
		pendingID = t.PendingID
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO ledger_transfers (id, kind, debit, credit, amount, pending_id, ref)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT DO NOTHING`,
		t.ID, int(t.Kind), string(t.Debit), string(t.Credit), int64(t.Amount), pendingID, t.Ref)
	if err != nil {
		if isFK(err) {
			return Transfer{}, false, ErrNoAccount
		}
		return Transfer{}, false, err
	}
	if tag.RowsAffected() == 0 {
		// Either the ID exists (replay) or the pending was already resolved.
		if s, found, err := getTx(ctx, tx, t.ID, false); err != nil {
			return Transfer{}, false, err
		} else if found {
			return replayOrReuse(t, s)
		}
		if t.Kind == Post || t.Kind == Void {
			return Transfer{}, false, ErrPendingResolved
		}
		return Transfer{}, false, errRetry
	}

	amt := int64(t.Amount)
	var q1, q2 string
	var a1, a2 int64
	switch t.Kind {
	case Single:
		q1, a1 = `UPDATE ledger_accounts SET debits_posted = debits_posted + $2 WHERE id = $1`, amt
		q2, a2 = `UPDATE ledger_accounts SET credits_posted = credits_posted + $2 WHERE id = $1`, amt
	case Pending:
		q1, a1 = `UPDATE ledger_accounts SET debits_pending = debits_pending + $2 WHERE id = $1`, amt
		q2, a2 = `UPDATE ledger_accounts SET credits_pending = credits_pending + $2 WHERE id = $1`, amt
	case Post:
		q1 = `UPDATE ledger_accounts SET debits_pending = debits_pending - $3, debits_posted = debits_posted + $2 WHERE id = $1`
		q2 = `UPDATE ledger_accounts SET credits_pending = credits_pending - $3, credits_posted = credits_posted + $2 WHERE id = $1`
		a1, a2 = amt, amt
	case Void:
		q1 = `UPDATE ledger_accounts SET debits_pending = debits_pending - $3 WHERE id = $1 AND $2::bigint >= 0`
		q2 = `UPDATE ledger_accounts SET credits_pending = credits_pending - $3 WHERE id = $1 AND $2::bigint >= 0`
	}
	args1 := []any{string(t.Debit), a1}
	args2 := []any{string(t.Credit), a2}
	if t.Kind == Post || t.Kind == Void {
		args1 = append(args1, int64(pend.Amount))
		args2 = append(args2, int64(pend.Amount))
	}
	if _, err := tx.Exec(ctx, q1, args1...); err != nil {
		if isCheck(err) {
			return Transfer{}, false, ErrInsufficientFunds
		}
		return Transfer{}, false, err
	}
	if _, err := tx.Exec(ctx, q2, args2...); err != nil {
		return Transfer{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Transfer{}, false, err
	}
	return t, false, nil
}

func replayOrReuse(req, stored Transfer) (Transfer, bool, error) {
	if !sameBody(req, stored) {
		return Transfer{}, false, ErrIDReuse
	}
	return stored, true, nil
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func getTx(ctx context.Context, q querier, id string, forUpdate bool) (Transfer, bool, error) {
	sql := `SELECT id, kind, debit, credit, amount, COALESCE(pending_id, ''), ref FROM ledger_transfers WHERE id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	var t Transfer
	var kind int
	var debit, credit string
	var amt int64
	err := q.QueryRow(ctx, sql, id).Scan(&t.ID, &kind, &debit, &credit, &amt, &t.PendingID, &t.Ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return Transfer{}, false, nil
	}
	if err != nil {
		return Transfer{}, false, err
	}
	t.Kind, t.Debit, t.Credit, t.Amount = TransferKind(kind), AccountID(debit), AccountID(credit), money.Paise(amt)
	return t, true, nil
}

// Get implements Ledger.
func (p *Postgres) Get(ctx context.Context, id string) (Transfer, bool, error) {
	return getTx(ctx, p.pool, id, false)
}

// Transfers implements Ledger.
func (p *Postgres) Transfers(ctx context.Context) ([]Transfer, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, kind, debit, credit, amount, COALESCE(pending_id, ''), ref FROM ledger_transfers ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Transfer
	for rows.Next() {
		var t Transfer
		var kind int
		var debit, credit string
		var amt int64
		if err := rows.Scan(&t.ID, &kind, &debit, &credit, &amt, &t.PendingID, &t.Ref); err != nil {
			return nil, err
		}
		t.Kind, t.Debit, t.Credit, t.Amount = TransferKind(kind), AccountID(debit), AccountID(credit), money.Paise(amt)
		out = append(out, t)
	}
	return out, rows.Err()
}

// Balance implements Ledger.
func (p *Postgres) Balance(ctx context.Context, id AccountID) (Balance, error) {
	var b [4]int64
	err := p.pool.QueryRow(ctx, `SELECT debits_posted, credits_posted, debits_pending, credits_pending FROM ledger_accounts WHERE id = $1`, string(id)).Scan(&b[0], &b[1], &b[2], &b[3])
	if errors.Is(err, pgx.ErrNoRows) {
		return Balance{}, ErrNoAccount
	}
	return Balance{money.Paise(b[0]), money.Paise(b[1]), money.Paise(b[2]), money.Paise(b[3])}, err
}

// CheckInvariants implements Ledger by replaying the transfer log.
func (p *Postgres) CheckInvariants(ctx context.Context) error {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	accounts := map[AccountID]AccountKind{}
	have := map[AccountID]Balance{}
	rows, err := tx.Query(ctx, `SELECT id, kind, debits_posted, credits_posted, debits_pending, credits_pending FROM ledger_accounts`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var kind int
		var b [4]int64
		if err := rows.Scan(&id, &kind, &b[0], &b[1], &b[2], &b[3]); err != nil {
			rows.Close()
			return err
		}
		accounts[AccountID(id)] = AccountKind(kind)
		have[AccountID(id)] = Balance{money.Paise(b[0]), money.Paise(b[1]), money.Paise(b[2]), money.Paise(b[3])}
	}
	rows.Close()
	transfers := map[string]Transfer{}
	var order []string
	rows, err = tx.Query(ctx, `SELECT id, kind, debit, credit, amount, COALESCE(pending_id, ''), ref FROM ledger_transfers ORDER BY seq`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var t Transfer
		var kind int
		var debit, credit string
		var amt int64
		if err := rows.Scan(&t.ID, &kind, &debit, &credit, &amt, &t.PendingID, &t.Ref); err != nil {
			rows.Close()
			return err
		}
		t.Kind, t.Debit, t.Credit, t.Amount = TransferKind(kind), AccountID(debit), AccountID(credit), money.Paise(amt)
		transfers[t.ID] = t
		order = append(order, t.ID)
	}
	rows.Close()
	return compare(accounts, func(id AccountID) Balance { return have[id] }, replay(order, transfers))
}
