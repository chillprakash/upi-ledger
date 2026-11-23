package psp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chillprakash/upi-ledger/internal/money"
)

// Store persists payments. The engine writes through it on every change, so
// a restarted engine (Restore) continues where a crashed one stopped. The
// idempotency key is unique in the store, which is what makes retries safe
// across processes and restarts.
type Store interface {
	// Insert stores p unless its idempotency key exists; then it returns
	// the stored payment and inserted=false.
	Insert(ctx context.Context, p Payment) (stored Payment, inserted bool, err error)
	Update(ctx context.Context, p Payment) error
	Load(ctx context.Context) ([]Payment, error)
}

// MemStore is an in-memory Store that outlives an Engine, so the simulator
// can crash an engine and restart a new one on the same data.
type MemStore struct {
	mu    sync.Mutex
	byID  map[string]Payment
	byKey map[string]string
}

// NewMemStore returns an empty store.
func NewMemStore() *MemStore {
	return &MemStore{byID: map[string]Payment{}, byKey: map[string]string{}}
}

// Insert implements Store.
func (m *MemStore) Insert(_ context.Context, p Payment) (Payment, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.byKey[p.IdempotencyKey]; ok {
		return clone(ptr(m.byID[id])), false, nil
	}
	m.byID[p.ID] = clone(&p)
	m.byKey[p.IdempotencyKey] = p.ID
	return p, true, nil
}

// Update implements Store.
func (m *MemStore) Update(_ context.Context, p Payment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byID[p.ID]; !ok {
		return fmt.Errorf("psp store: no payment %s", p.ID)
	}
	m.byID[p.ID] = clone(&p)
	return nil
}

// Load implements Store.
func (m *MemStore) Load(context.Context) ([]Payment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Payment, 0, len(m.byID))
	for _, p := range m.byID {
		out = append(out, clone(ptr(p)))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func ptr(p Payment) *Payment { return &p }

// PGSchema is the app's payment table.
const PGSchema = `
CREATE TABLE IF NOT EXISTS psp_payments (
	id              text PRIMARY KEY,
	idempotency_key text NOT NULL UNIQUE,
	txn_id          text NOT NULL UNIQUE,
	payer           text NOT NULL,
	payee           text NOT NULL,
	amount          bigint NOT NULL CHECK (amount > 0),
	kind            text NOT NULL,
	merchant        boolean NOT NULL,
	payee_name      text NOT NULL,
	state           text NOT NULL,
	code            text NOT NULL DEFAULT '',
	checks          int NOT NULL DEFAULT 0 CHECK (checks BETWEEN 0 AND 3),
	next_check      timestamptz,
	recon_required  boolean NOT NULL DEFAULT false,
	created_at      timestamptz NOT NULL,
	updated_at      timestamptz NOT NULL,
	history         text[] NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS psp_payments_due ON psp_payments (next_check) WHERE state IN ('pending', 'deemed', 'initiated');
`

// PGStore is a Store in Postgres.
type PGStore struct{ pool *pgxpool.Pool }

// NewPGStore creates the table if needed.
func NewPGStore(ctx context.Context, pool *pgxpool.Pool) (*PGStore, error) {
	if _, err := pool.Exec(ctx, PGSchema); err != nil {
		return nil, fmt.Errorf("psp schema: %w", err)
	}
	return &PGStore{pool: pool}, nil
}

const pgCols = `id, idempotency_key, txn_id, payer, payee, amount, kind, merchant, payee_name, state, code, checks, next_check, recon_required, created_at, updated_at, history`

// Insert implements Store. ON CONFLICT on the idempotency key makes two
// processes racing on one key agree on a single payment.
func (s *PGStore) Insert(ctx context.Context, p Payment) (Payment, bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO psp_payments (`+pgCols+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		ON CONFLICT (idempotency_key) DO NOTHING`, args(p)...)
	if err != nil {
		return Payment{}, false, err
	}
	if tag.RowsAffected() == 1 {
		return p, true, nil
	}
	row := s.pool.QueryRow(ctx, `SELECT `+pgCols+` FROM psp_payments WHERE idempotency_key = $1`, p.IdempotencyKey)
	got, err := scan(row)
	return got, false, err
}

// Update implements Store.
func (s *PGStore) Update(ctx context.Context, p Payment) error {
	tag, err := s.pool.Exec(ctx, `UPDATE psp_payments SET state=$2, code=$3, checks=$4, next_check=$5,
		recon_required=$6, updated_at=$7, history=$8, payee_name=$9 WHERE id=$1`,
		p.ID, string(p.State), p.Code, p.Checks, nullTime(p.NextCheck), p.ReconRequired, p.UpdatedAt, nonNil(p.History), p.PayeeName)
	if err == nil && tag.RowsAffected() != 1 {
		err = fmt.Errorf("psp store: no payment %s", p.ID)
	}
	return err
}

// Load implements Store.
func (s *PGStore) Load(ctx context.Context) ([]Payment, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+pgCols+` FROM psp_payments ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Payment
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func args(p Payment) []any {
	if p.History == nil {
		p.History = []string{}
	}
	return []any{p.ID, p.IdempotencyKey, p.TxnID, p.Request.Payer, p.Request.Payee, int64(p.Request.Amount),
		string(p.Request.Kind), p.Request.Merchant, p.PayeeName, string(p.State), p.Code, p.Checks,
		nullTime(p.NextCheck), p.ReconRequired, p.CreatedAt, p.UpdatedAt, p.History}
}

func scan(row pgx.Row) (Payment, error) {
	var p Payment
	var amt int64
	var kind, state string
	var next *time.Time
	err := row.Scan(&p.ID, &p.IdempotencyKey, &p.TxnID, &p.Request.Payer, &p.Request.Payee, &amt, &kind,
		&p.Request.Merchant, &p.PayeeName, &state, &p.Code, &p.Checks, &next, &p.ReconRequired,
		&p.CreatedAt, &p.UpdatedAt, &p.History)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payment{}, fmt.Errorf("psp store: payment vanished: %w", err)
	}
	p.Request.Amount, p.Request.Kind, p.State = money.Paise(amt), Kind(kind), State(state)
	if next != nil {
		p.NextCheck = *next
	}
	return p, err
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func nonNil(h []string) []string {
	if h == nil {
		return []string{}
	}
	return h
}
