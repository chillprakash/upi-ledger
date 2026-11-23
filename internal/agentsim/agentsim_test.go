package agentsim

import (
	"os"
	"strconv"
	"testing"
)

func TestAgentSeeds(t *testing.T) {
	n := 200
	if s := os.Getenv("UPI_AGENT_SEEDS"); s != "" {
		n, _ = strconv.Atoi(s)
	}
	for seed := uint64(1); seed <= uint64(n); seed++ {
		r := Run(seed, 40)
		if !r.OK() {
			t.Fatalf("seed %d: %d violations, first: %s", seed, len(r.Violations), r.Violations[0])
		}
	}
}

func TestAgentDeterministic(t *testing.T) {
	a, b := Run(7, 40), Run(7, 40)
	if a.Orders != b.Orders || a.Faults["lost_response"] != b.Faults["lost_response"] || a.Paid["honest"] != b.Paid["honest"] {
		t.Fatalf("same seed, different runs: %+v / %+v", a, b)
	}
}
