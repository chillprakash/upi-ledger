package ledger_test

import (
	"context"
	"testing"

	"github.com/chillprakash/upi-ledger/internal/ledger"
	"github.com/chillprakash/upi-ledger/internal/ledger/ledgertest"
	"github.com/chillprakash/upi-ledger/internal/pgtest"
)

func TestMemory(t *testing.T) {
	ledgertest.Run(t, func(*testing.T) ledger.Ledger { return ledger.NewMemory() })
}

func TestPostgres(t *testing.T) {
	ledgertest.Run(t, func(t *testing.T) ledger.Ledger {
		l, err := ledger.NewPostgres(context.Background(), pgtest.Pool(t))
		if err != nil {
			t.Fatal(err)
		}
		return l
	})
}
