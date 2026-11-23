// Command upi-sim runs seeded simulations of the whole UPI path and reports
// faults injected, outcomes and invariant violations.
//
//	go run ./cmd/upi-sim -seeds 10000          # sweep seeds 1..10000
//	go run ./cmd/upi-sim -seed 42 -v           # replay one seed in detail
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/chillprakash/upi-ledger/internal/sim"
)

func main() {
	seeds := flag.Int("seeds", 1000, "number of seeds to run, starting at -from")
	from := flag.Uint64("from", 1, "first seed")
	one := flag.Uint64("seed", 0, "run only this seed")
	payments := flag.Int("payments", 0, "payments per run (default 60)")
	verbose := flag.Bool("v", false, "print each run's report")
	asJSON := flag.Bool("json", false, "print the summary as JSON")
	flag.Parse()

	cfg := sim.Config{Payments: *payments}
	if *one != 0 {
		r := sim.Run(*one, cfg)
		b, _ := json.MarshalIndent(r, "", "  ")
		fmt.Println(string(b))
		if !r.OK() {
			os.Exit(1)
		}
		return
	}

	start := time.Now()
	type agg struct {
		sync.Mutex
		Runs, Payments, Checks, Flagged, Duplicates, KeyReuse              int
		Crashes, Recovered, Outages, OutageCalls, Refunds, RefundsRejected int
		Files, Lines, Compensated                                          int
		Compensation                                                       int64
		Lite, BlockDebits, BlockDeclines, ChatOrders, ChatRetaps           int
		States, Switch, Faults, Corrections                                map[string]int
		SimTime                                                            time.Duration
		Failures                                                           []string
	}
	a := &agg{States: map[string]int{}, Switch: map[string]int{}, Faults: map[string]int{}, Corrections: map[string]int{}}
	work := make(chan uint64)
	var wg sync.WaitGroup
	for w := 0; w < runtime.NumCPU(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range work {
				r := sim.Run(s, cfg)
				if *verbose {
					fmt.Printf("seed %d ok=%v payments=%d states=%v\n", s, r.OK(), r.Payments, r.ByState)
				}
				a.Lock()
				a.Runs++
				a.Payments += r.Payments
				a.Checks += r.StatusChecks
				a.Flagged += r.ReconFlagged
				a.Duplicates += r.DuplicateSubmits
				a.KeyReuse += r.KeyReuseRejected
				a.SimTime += r.SimulatedDuration
				a.Crashes += r.Crashes
				a.Recovered += r.Recovered
				if r.Outage {
					a.Outages++
				}
				a.OutageCalls += r.OutageCalls
				a.Refunds += r.Refunds
				a.RefundsRejected += r.RefundsRejected
				a.Files += r.SettlementFiles
				a.Lines += r.SettlementLines
				a.Compensated += r.CompensatedTxns
				a.Compensation += int64(r.CompensationPaid)
				a.Lite += r.LitePayments
				a.BlockDebits += r.BlockDebits
				a.BlockDeclines += r.BlockDeclines
				a.ChatOrders += r.ChatOrders
				a.ChatRetaps += r.ChatRetaps
				for k, v := range r.ByState {
					a.States[k] += v
				}
				for k, v := range r.BySwitchStatus {
					a.Switch[k] += v
				}
				for k, v := range r.Faults {
					a.Faults[k] += v
				}
				for k, v := range r.ReconCorrections {
					a.Corrections[k] += v
				}
				if !r.OK() {
					a.Failures = append(a.Failures, fmt.Sprintf("seed %d: %s", s, r.Violations[0]))
				}
				a.Unlock()
			}
		}()
	}
	for s := *from; s < *from+uint64(*seeds); s++ {
		work <- s
	}
	close(work)
	wg.Wait()
	sort.Strings(a.Failures)

	if *asJSON {
		b, _ := json.MarshalIndent(a, "", "  ")
		fmt.Println(string(b))
	} else {
		fmt.Printf("runs %d  payments %d  wall %s  simulated %s\n", a.Runs, a.Payments, time.Since(start).Round(time.Millisecond), a.SimTime.Round(time.Hour))
		fmt.Printf("faults injected   %v\n", a.Faults)
		fmt.Printf("app final states  %v\n", a.States)
		fmt.Printf("switch statuses   %v\n", a.Switch)
		fmt.Printf("duplicate submits %d  key reuse refused %d  status checks %d\n", a.Duplicates, a.KeyReuse, a.Checks)
		fmt.Printf("flagged by Annexure C %d  corrected by reconciliation %v\n", a.Flagged, a.Corrections)
		fmt.Printf("app crashes %d  recovered mid-flight %d\n", a.Crashes, a.Recovered)
		fmt.Printf("bank outages %d runs (%d calls refused)  refunds ok %d, refused %d\n", a.Outages, a.OutageCalls, a.Refunds, a.RefundsRejected)
		fmt.Printf("settlement files %d (%d lines)  RBI compensation Rs %d to %d payers\n", a.Files, a.Lines, a.Compensation/100, a.Compensated)
		fmt.Printf("UPI LITE payments %d  Reserve Pay debits %d (declined %d)  chat orders %d (extra taps %d)\n", a.Lite, a.BlockDebits, a.BlockDeclines, a.ChatOrders, a.ChatRetaps)
		fmt.Printf("invariant violations: %d runs\n", len(a.Failures))
		for i, f := range a.Failures {
			if i == 10 {
				fmt.Printf("  ... %d more\n", len(a.Failures)-10)
				break
			}
			fmt.Println("  " + f)
		}
	}
	if len(a.Failures) > 0 {
		os.Exit(1)
	}
}
