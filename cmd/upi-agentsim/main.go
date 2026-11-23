// Command upi-agentsim sweeps seeded agent-commerce simulations: honest and
// adversarial agents paying through ACP with UPI allowances, under faults.
//
//	go run ./cmd/upi-agentsim -seeds 5000
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/chillprakash/upi-ledger/internal/agentsim"
)

func main() {
	seeds := flag.Int("seeds", 1000, "number of seeds")
	sessions := flag.Int("sessions", 40, "agent sessions per run")
	flag.Parse()
	start := time.Now()
	var mu sync.Mutex
	sess, paid, refused, faults := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	orders := 0
	var fails []string
	work := make(chan uint64)
	var wg sync.WaitGroup
	for i := 0; i < runtime.NumCPU(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range work {
				r := agentsim.Run(s, *sessions)
				mu.Lock()
				for k, v := range r.Sessions {
					sess[k] += v
				}
				for k, v := range r.Paid {
					paid[k] += v
				}
				for k, v := range r.Refused {
					refused[k] += v
				}
				for k, v := range r.Faults {
					faults[k] += v
				}
				orders += r.Orders
				if !r.OK() {
					fails = append(fails, fmt.Sprintf("seed %d: %s", s, r.Violations[0]))
				}
				mu.Unlock()
			}
		}()
	}
	for s := uint64(1); s <= uint64(*seeds); s++ {
		work <- s
	}
	close(work)
	wg.Wait()
	sort.Strings(fails)
	fmt.Printf("runs %d  wall %s  orders %d\n", *seeds, time.Since(start).Round(time.Millisecond), orders)
	fmt.Printf("faults %v\n", faults)
	fmt.Printf("%-16s %9s %9s %9s\n", "policy", "sessions", "paid", "refused")
	for _, p := range agentsim.Policies {
		fmt.Printf("%-16s %9d %9d %9d\n", p, sess[p], paid[p], refused[p])
	}
	fmt.Printf("invariant violations: %d runs\n", len(fails))
	for i, f := range fails {
		if i == 10 {
			break
		}
		fmt.Println("  " + f)
	}
	if len(fails) > 0 {
		os.Exit(1)
	}
}
