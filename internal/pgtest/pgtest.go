// Package pgtest gives each test its own Postgres schema on the project's
// dev database, and skips when Postgres is not reachable.
package pgtest

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultURL is the shared machine Postgres (`ports claim shared/postgres`, 5432).
const DefaultURL = "postgres://upi@127.0.0.1:5432/upi_ledger_test?sslmode=disable"

// Pool returns a pool whose search_path is a fresh schema, dropped at cleanup.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("UPI_TEST_DATABASE_URL")
	if url == "" {
		url = DefaultURL
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err == nil {
		err = admin.Ping(ctx)
	}
	if err != nil {
		if os.Getenv("UPI_REQUIRE_PG") != "" {
			t.Fatalf("postgres not reachable: %v", err)
		}
		t.Skipf("postgres not reachable (%v); set UPI_TEST_DATABASE_URL", err)
	}
	schema := fmt.Sprintf("t_%d", rand.Uint64())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 32
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") //nolint:errcheck
		admin.Close()
	})
	return pool
}
