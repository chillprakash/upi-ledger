// Package clock gives every rule that depends on time (60 s status checks,
// 2 h windows, T+1 reversals) one injectable source, so tests and the
// simulator run hours of rule time in microseconds.
package clock

import (
	"sync"
	"time"
)

// Clock reports the current time.
type Clock interface{ Now() time.Time }

// Real is the wall clock.
type Real struct{}

// Now returns time.Now().
func (Real) Now() time.Time { return time.Now() }

// Sim is a manually advanced clock. Safe for concurrent use.
type Sim struct {
	mu  sync.Mutex
	now time.Time
}

// NewSim starts a simulated clock at t.
func NewSim(t time.Time) *Sim { return &Sim{now: t} }

// Now returns the simulated time.
func (s *Sim) Now() time.Time { s.mu.Lock(); defer s.mu.Unlock(); return s.now }

// Advance moves the clock forward by d.
func (s *Sim) Advance(d time.Duration) { s.mu.Lock(); s.now = s.now.Add(d); s.mu.Unlock() }

// Set moves the clock to t if t is later than now.
func (s *Sim) Set(t time.Time) {
	s.mu.Lock()
	if t.After(s.now) {
		s.now = t
	}
	s.mu.Unlock()
}
