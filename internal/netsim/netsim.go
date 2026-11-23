// Package netsim is the simulated network between the app (PSP), the NPCI
// switch and the banks. Every call goes through it, and a seeded fault plan
// decides what happens to it. Time is a simulated clock, so a 15 s deadline
// costs nothing to test.
//
// What a fault means for the caller:
//
//	ConnectFail   request never delivered; caller sees ErrConnect
//	LostResponse  request processed, response lost; caller sees ErrTimeout
//	Slow          request processed after the deadline; caller sees ErrTimeout
//	Duplicate     request delivered twice; caller sees the first response
//
// LostResponse and Slow are the dangerous ones: the other side did the work
// but the caller cannot know. They are why UPI needs status checks,
// reversals and reconciliation.
package netsim

import (
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/chillprakash/upi-ledger/internal/clock"
)

// FaultKind is what the network does to one call.
type FaultKind int

const (
	OK FaultKind = iota
	ConnectFail
	LostResponse
	Slow
	Duplicate
	numKinds
)

func (k FaultKind) String() string {
	return [...]string{"ok", "connect_fail", "lost_response", "slow", "duplicate"}[k]
}

// Errors seen by callers.
var (
	ErrConnect = errors.New("netsim: connection failed")
	ErrTimeout = errors.New("netsim: deadline exceeded")
)

// Plan decides the fault for each call.
type Plan interface {
	Next(link string) FaultKind
}

// NoFaults delivers everything.
type NoFaults struct{}

// Next implements Plan.
func (NoFaults) Next(string) FaultKind { return OK }

// Random injects faults with fixed per-kind probabilities from a seeded source.
type Random struct {
	mu      sync.Mutex
	rng     *rand.Rand
	P       [numKinds]float64 // probability of each non-OK kind
	Healed  bool              // when true, no faults (the network has recovered)
	Counted [numKinds]int
}

// NewRandom builds a plan; p gives probabilities for ConnectFail, LostResponse,
// Slow and Duplicate.
func NewRandom(rng *rand.Rand, connect, lost, slow, dup float64) *Random {
	r := &Random{rng: rng}
	r.P[ConnectFail], r.P[LostResponse], r.P[Slow], r.P[Duplicate] = connect, lost, slow, dup
	return r
}

// Next implements Plan.
func (r *Random) Next(string) FaultKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := OK
	if !r.Healed {
		x := r.rng.Float64()
		for kind := ConnectFail; kind < numKinds; kind++ {
			if x < r.P[kind] {
				k = kind
				break
			}
			x -= r.P[kind]
		}
	}
	r.Counted[k]++
	return k
}

// Net carries calls under a plan and advances the clock by their latency.
// With a nil Clock (real time) nothing is advanced.
type Net struct {
	Clock *clock.Sim
	Plan  Plan
	// Latency of a normal call; Slow calls take the deadline plus this.
	Latency time.Duration
}

// Call runs fn on the far side according to the plan. deadline is the
// caller's timeout for this API (NPCI 12.1: 15 s pay legs, 10 s status).
func Call[T any](n *Net, link string, deadline time.Duration, fn func() T) (T, error) {
	var zero T
	switch n.Plan.Next(link) {
	case ConnectFail:
		n.advance(n.Latency)
		return zero, ErrConnect
	case LostResponse:
		fn()
		n.advance(deadline)
		return zero, ErrTimeout
	case Slow:
		n.advance(deadline + n.Latency)
		fn()
		return zero, ErrTimeout
	case Duplicate:
		r := fn()
		fn()
		n.advance(n.Latency)
		return r, nil
	default:
		r := fn()
		n.advance(n.Latency)
		return r, nil
	}
}

// Script plays a fixed list of faults per link, then OK. For exact tests.
type Script struct {
	mu     sync.Mutex
	Faults map[string][]FaultKind
}

// Next implements Plan.
func (s *Script) Next(link string) FaultKind {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.Faults[link]
	if len(q) == 0 {
		return OK
	}
	s.Faults[link] = q[1:]
	return q[0]
}

// Window is a period when a link is down.
type Window struct{ From, To time.Time }

// Outages wraps a plan: during a link's down windows every call to it fails
// to connect (a bank outage). Outside them the inner plan decides.
type Outages struct {
	Inner Plan
	Clock clock.Clock
	Down  map[string][]Window
	Hits  int
}

// Next implements Plan.
func (o *Outages) Next(link string) FaultKind {
	now := o.Clock.Now()
	for _, w := range o.Down[link] {
		if !now.Before(w.From) && now.Before(w.To) {
			o.Hits++
			return ConnectFail
		}
	}
	return o.Inner.Next(link)
}

func (n *Net) advance(d time.Duration) {
	if n.Clock != nil {
		n.Clock.Advance(d)
	}
}
