package sim

import (
	"os"
	"strconv"
	"testing"
)

// TestSeeds runs many seeded simulations; any failure names its seed so it
// can be replayed with: go run ./cmd/upi-sim -seed N -v
func TestSeeds(t *testing.T) {
	n := 300
	if s := os.Getenv("UPI_SIM_SEEDS"); s != "" {
		n, _ = strconv.Atoi(s)
	}
	for seed := uint64(1); seed <= uint64(n); seed++ {
		r := Run(seed, Config{})
		if !r.OK() {
			t.Fatalf("seed %d: %d violations, first: %s", seed, len(r.Violations), r.Violations[0])
		}
	}
}

func TestDeterministic(t *testing.T) {
	a, b := Run(42, Config{}), Run(42, Config{})
	if a.Payments != b.Payments || a.StatusChecks != b.StatusChecks || a.SimulatedDuration != b.SimulatedDuration ||
		a.Faults["lost_response"] != b.Faults["lost_response"] || a.ByState["success"] != b.ByState["success"] {
		t.Fatalf("same seed, different runs:\n%+v\n%+v", a, b)
	}
}
