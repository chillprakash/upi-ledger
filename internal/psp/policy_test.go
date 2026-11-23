package psp_test

import (
	"errors"
	"testing"
	"time"

	"github.com/chillprakash/upi-ledger/internal/money"
	"github.com/chillprakash/upi-ledger/internal/psp"
)

var ist = time.FixedZone("IST", 19800)

func TestChooseLite(t *testing.T) {
	cases := []struct {
		amt, mirror money.Paise
		enabled     bool
		want        bool
	}{
		{money.Rupees(499), money.Rupees(1000), true, true},
		{money.Rupees(500), money.Rupees(1000), true, false}, // only below Rs 500 is mandatory
		{money.Rupees(300), money.Rupees(200), true, false},  // LITE cannot cover: fall back
		{money.Rupees(300), money.Rupees(1000), false, false},
	}
	for _, c := range cases {
		if got := psp.ChooseLite(c.amt, c.enabled, c.mirror, true); got != c.want {
			t.Errorf("%+v: got %v", c, got)
		}
	}
}

func TestHandles(t *testing.T) {
	if _, err := psp.NewHandles("a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"); !errors.Is(err, psp.ErrPartners) {
		t.Fatal("11 partner banks allowed")
	}
	h, _ := psp.NewHandles("icici", "hdfc", "axis", "sbi")
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, ist)
	if got := h.Allocate("hdfc", now); got != "hdfc" {
		t.Fatalf("own bank: %s", got)
	}
	counts := map[string]int{}
	for i := 0; i < 400; i++ {
		counts[h.Allocate("kotak", now)]++
	}
	for _, b := range []string{"axis", "icici", "sbi"} {
		if counts[b] < 99 || counts[b] > 101 {
			t.Fatalf("uneven allocation: %v", counts)
		}
	}
	if !psp.MultiBankRequired(0.051) || psp.MultiBankRequired(0.05) {
		t.Fatal("5% threshold")
	}
	if r := h.Report(); r["2026-11"]["hdfc"] == 0 {
		t.Fatalf("report %v", r)
	}
}

func TestLimiter(t *testing.T) {
	l := psp.NewLimiter()
	t0 := time.Date(2026, 11, 23, 9, 0, 0, 0, ist)
	for i := 0; i < 50; i++ {
		if err := l.Allow("asha", "balance", psp.BalanceEnquiryLimit, t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Allow("asha", "balance", psp.BalanceEnquiryLimit, t0.Add(23*time.Hour)); !errors.Is(err, psp.ErrLimit) {
		t.Fatalf("51st balance enquiry: %v", err)
	}
	if err := l.Allow("asha", "balance", psp.BalanceEnquiryLimit, t0.Add(24*time.Hour)); err != nil {
		t.Fatalf("window rolls from the first request: %v", err)
	}
	if err := l.Allow("ravi", "balance", psp.BalanceEnquiryLimit, t0); err != nil {
		t.Fatal("limits are per user")
	}
}

func TestPeakHours(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 23, h, m, 0, 0, ist) }
	if !psp.IsPeak(at(10, 0), ist) || psp.IsPeak(at(13, 0), ist) || !psp.IsPeak(at(21, 29), ist) || psp.IsPeak(at(21, 30), ist) {
		t.Fatal("peak windows")
	}
	if got := psp.NextOffPeak(at(18, 0), ist); !got.Equal(at(21, 30)) {
		t.Fatalf("next off-peak %s", got)
	}
}
