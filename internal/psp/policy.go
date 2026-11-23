package psp

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/chillprakash/upi-ledger/internal/money"
)

// LiteMandatoryBelow: for a user with UPI LITE enabled (and only savings or
// current accounts linked), payments under Rs 500 must go through LITE
// (Consolidated Circular 18.1).
var LiteMandatoryBelow = money.Rupees(500)

// ChooseLite decides whether a payment uses UPI LITE. mirror is the LITE
// balance mirrored on the device (the app keeps no balances of its own,
// 12.8; the device's Common Library holds the mirror, 18). If LITE cannot
// cover a small payment, the app falls back to the bank account.
func ChooseLite(amount money.Paise, liteEnabled bool, mirror money.Paise, onlyCASA bool) bool {
	return liteEnabled && onlyCASA && amount < LiteMandatoryBelow && amount <= mirror
}

// MaxPSPBanks is the most partner banks a TPAP may use (2.10).
const MaxPSPBanks = 10

// MultiBankShare is the monthly UPI share above which a TPAP must use more
// than one partner bank (2.10).
const MultiBankShare = 0.05

// Handles allocates UPI handles across a TPAP's partner banks (2.10, 2.11):
// a user whose own bank is a partner gets that bank's handle; everyone else
// is spread evenly. Counts feed the yearly allocation report.
type Handles struct {
	mu       sync.Mutex
	partners []string
	counts   map[string]int
	byMonth  map[string]map[string]int
}

// ErrPartners is returned for an invalid partner list.
var ErrPartners = errors.New("psp: a TPAP needs 1 to 10 partner banks")

// NewHandles validates the partner list.
func NewHandles(partners ...string) (*Handles, error) {
	if len(partners) == 0 || len(partners) > MaxPSPBanks {
		return nil, ErrPartners
	}
	p := append([]string(nil), partners...)
	sort.Strings(p)
	return &Handles{partners: p, counts: map[string]int{}, byMonth: map[string]map[string]int{}}, nil
}

// MultiBankRequired reports whether a TPAP with this share must use more than one bank.
func MultiBankRequired(share float64) bool { return share > MultiBankShare }

// Allocate picks the partner bank whose handle the user gets.
func (h *Handles) Allocate(userBank string, at time.Time) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	pick := ""
	for _, p := range h.partners {
		if p == userBank {
			pick = p
		}
	}
	if pick == "" { // fair (equal distribution): least-allocated partner
		for _, p := range h.partners {
			if pick == "" || h.counts[p] < h.counts[pick] {
				pick = p
			}
		}
	}
	h.counts[pick]++
	m := at.Format("2006-01")
	if h.byMonth[m] == nil {
		h.byMonth[m] = map[string]int{}
	}
	h.byMonth[m][pick]++
	return pick
}

// Report is the month-wise handle allocation report (2.11).
func (h *Handles) Report() map[string]map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]map[string]int{}
	for m, c := range h.byMonth {
		out[m] = map[string]int{}
		for k, v := range c {
			out[m][k] = v
		}
	}
	return out
}

// API usage limits (12.8). Windows roll from the first successful request.
var (
	BalanceEnquiryLimit = 50
	ListAccountLimit    = 25
	Window              = 24 * time.Hour
)

// ErrLimit is returned when a user hits an API limit.
var ErrLimit = errors.New("psp: API usage limit reached for this user (12.8)")

// Limiter enforces per-user, per-API daily limits.
type Limiter struct {
	mu   sync.Mutex
	wins map[string]*window
}

type window struct {
	start time.Time
	count int
}

// NewLimiter returns an empty limiter.
func NewLimiter() *Limiter { return &Limiter{wins: map[string]*window{}} }

// Allow records a user-initiated call to api if within its limit.
func (l *Limiter) Allow(user, api string, limit int, now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := user + "|" + api
	w := l.wins[k]
	if w == nil || now.Sub(w.start) >= Window {
		w = &window{start: now}
		l.wins[k] = w
	}
	if w.count >= limit {
		return fmt.Errorf("%w: %s %d per %s", ErrLimit, api, limit, Window)
	}
	w.count++
	return nil
}

// PeakHours are when non-user-initiated APIs must be restricted (12.9.2).
// NPCI defines the windows separately; these defaults (10:00-13:00 and
// 17:00-21:30 IST) follow industry reporting and are configurable.
var PeakHours = [][2]time.Duration{{10 * time.Hour, 13 * time.Hour}, {17 * time.Hour, 21*time.Hour + 30*time.Minute}}

// IsPeak reports whether t (in IST) is in peak hours.
func IsPeak(t time.Time, ist *time.Location) bool {
	t = t.In(ist)
	d := time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute
	for _, w := range PeakHours {
		if d >= w[0] && d < w[1] {
			return true
		}
	}
	return false
}

// NextOffPeak returns the first moment at or after t outside peak hours,
// for scheduling system-initiated work such as AutoPay executions.
func NextOffPeak(t time.Time, ist *time.Location) time.Time {
	for IsPeak(t, ist) {
		t = t.Add(time.Minute)
	}
	return t
}
