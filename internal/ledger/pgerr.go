package ledger

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func isFK(err error) bool    { return pgCode(err) == "23503" }
func isCheck(err error) bool { return pgCode(err) == "23514" }
