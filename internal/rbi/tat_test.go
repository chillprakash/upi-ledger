package rbi_test

import (
	"testing"
	"time"

	"github.com/chillprakash/upi-ledger/internal/npci"
	"github.com/chillprakash/upi-ledger/internal/rbi"
)

func ist(d, h int) time.Time { return time.Date(2026, 11, d, h, 0, 0, 0, npci.IST) }

func TestDaysLate(t *testing.T) {
	p2p := npci.Record{CreatedAt: ist(23, 22), Status: npci.Reversed, Kind: npci.KindPay}
	cases := []struct {
		rev  time.Time
		want int
	}{
		{ist(23, 23), 0}, {ist(24, 23), 0}, // by end of T+1: on time
		{ist(25, 0), 1}, {ist(25, 23), 1}, {ist(26, 9), 2},
	}
	for _, c := range cases {
		r := p2p
		r.ReversedAt = c.rev
		if got := rbi.DaysLate(r, c.rev); got != c.want {
			t.Errorf("P2P reversed %s: %d days late, want %d", c.rev.Format("02 15:04"), got, c.want)
		}
	}
	m := npci.Record{CreatedAt: ist(23, 10), Status: npci.Reversed, Kind: npci.KindPay, Merchant: true, ReversedAt: ist(28, 23)}
	if got := rbi.DaysLate(m, m.ReversedAt); got != 0 {
		t.Errorf("merchant T+5: %d", got)
	}
	m.ReversedAt = ist(29, 1)
	if got := rbi.DaysLate(m, m.ReversedAt); got != 1 {
		t.Errorf("merchant T+6: %d", got)
	}
	// A deemed payment whose credit lands on T+3 is 2 days late.
	d := npci.Record{CreatedAt: ist(23, 10), Status: npci.Success, Kind: npci.KindPay, FinalAt: ist(26, 10)}
	if got := rbi.DaysLate(d, ist(30, 0)); got != 2 {
		t.Errorf("late credit: %d", got)
	}
}
