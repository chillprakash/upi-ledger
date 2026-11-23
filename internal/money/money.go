// Package money represents rupee amounts as integer paise. Floats never touch money.
package money

import (
	"fmt"
	"strconv"
	"strings"
)

// Paise is an amount in paise (1 rupee = 100 paise).
type Paise int64

// Rupees builds an amount from whole rupees.
func Rupees(r int64) Paise { return Paise(r * 100) }

// String formats as rupees with two decimals, e.g. "100.50".
func (p Paise) String() string {
	sign := ""
	v := int64(p)
	if v < 0 {
		sign, v = "-", -v
	}
	return fmt.Sprintf("%s%d.%02d", sign, v/100, v%100)
}

// Parse reads "100", "100.5" or "100.50" (UPI amount format) into paise.
func Parse(s string) (Paise, error) {
	s = strings.TrimSpace(s)
	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" || strings.HasPrefix(whole, "-") || strings.HasPrefix(whole, "+") {
		return 0, fmt.Errorf("money: invalid amount %q", s)
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("money: invalid amount %q", s)
	}
	var f int64
	if hasFrac {
		if len(frac) == 0 || len(frac) > 2 {
			return 0, fmt.Errorf("money: invalid amount %q", s)
		}
		if len(frac) == 1 {
			frac += "0"
		}
		if f, err = strconv.ParseInt(frac, 10, 64); err != nil || f < 0 {
			return 0, fmt.Errorf("money: invalid amount %q", s)
		}
	}
	if w > (1<<62)/100 {
		return 0, fmt.Errorf("money: amount too large %q", s)
	}
	return Paise(w*100 + f), nil
}
