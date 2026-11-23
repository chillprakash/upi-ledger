// Command upi-load runs the real system (HTTP API, Postgres ledgers and
// payment store) under concurrent load with duplicate submissions and
// network faults, then settles everything and checks every invariant
// against what is stored in Postgres.
//
//	go run ./cmd/upi-load -db postgres://upi@127.0.0.1:5432/upi_ledger -clients 32 -duration 30s -faults 0.1
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/chillprakash/upi-ledger/internal/world"
)

type result struct {
	Config struct {
		Clients  int     `json:"clients"`
		Duration string  `json:"duration"`
		Faults   float64 `json:"fault_rate"`
		Store    string  `json:"store"`
	} `json:"config"`
	Requests   int                `json:"requests"`
	Duplicates int                `json:"duplicate_requests"`
	HTTPStatus map[int]int        `json:"http_status"`
	Throughput float64            `json:"requests_per_second"`
	LatencyMs  map[string]float64 `json:"latency_ms"`
	Check      world.CheckReport  `json:"check"`
}

func main() {
	db := flag.String("db", "", "Postgres URL (empty: in memory)")
	clients := flag.Int("clients", 32, "concurrent clients")
	dur := flag.Duration("duration", 20*time.Second, "load duration")
	faults := flag.Float64("faults", 0.1, "share of network calls that fail")
	dup := flag.Float64("dup", 0.2, "share of requests that repeat an earlier key")
	out := flag.String("out", "", "write the JSON result here")
	keep := flag.Bool("keep", false, "keep the run's Postgres schemas")
	flag.Parse()

	ctx := context.Background()
	schema := fmt.Sprintf("load_%d", time.Now().Unix())
	w, err := world.Build(ctx, world.Config{DatabaseURL: *db, Schema: schema, Banks: 3, Users: 100, FaultRate: *faults, Speed: 60, Seed: 1})
	if err != nil {
		log.Fatal(err)
	}
	defer w.Close(ctx, !*keep)
	runCtx, cancel := context.WithCancel(ctx)
	go w.Run(runCtx)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Handler: w.Handler()}
	go srv.Serve(ln) //nolint:errcheck
	base := "http://" + ln.Addr().String()

	var mu sync.Mutex
	var lat []time.Duration
	var res result
	res.HTTPStatus = map[int]int{}
	type sent struct{ key, body string }
	var history []sent
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: *clients}}
	deadline := time.Now().Add(*dur)
	var wg sync.WaitGroup
	for c := 0; c < *clients; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(c), 99))
			for i := 0; time.Now().Before(deadline); i++ {
				var s sent
				mu.Lock()
				isDup := len(history) > 0 && rng.Float64() < *dup
				if isDup {
					s = history[rng.IntN(len(history))]
				}
				mu.Unlock()
				if !isDup {
					from, to := w.Users[rng.IntN(len(w.Users))], w.Users[rng.IntN(len(w.Users))]
					if from.VPA == to.VPA {
						continue
					}
					s.key = fmt.Sprintf("c%d-%d", c, i)
					s.body = fmt.Sprintf(`{"payer":%q,"payee":%q,"amount":"%d.%02d"}`, from.VPA, to.VPA, 1+rng.IntN(2000), rng.IntN(100))
				}
				req, _ := http.NewRequest("POST", base+"/payments", bytes.NewBufferString(s.body))
				req.Header.Set("Idempotency-Key", s.key)
				t0 := time.Now()
				resp, err := client.Do(req)
				d := time.Since(t0)
				code := 0
				if err == nil {
					code = resp.StatusCode
					resp.Body.Close()
				}
				mu.Lock()
				lat = append(lat, d)
				res.Requests++
				res.HTTPStatus[code]++
				if isDup {
					res.Duplicates++
				} else if code == 200 {
					history = append(history, s)
				}
				mu.Unlock()
			}
		}(c)
	}
	wg.Wait()
	elapsed := *dur
	cancel()
	srv.Shutdown(ctx) //nolint:errcheck

	log.Printf("load done: %d requests; settling...", res.Requests)
	w.Quiesce(ctx)
	res.Check = w.Check(ctx)

	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) float64 {
		if len(lat) == 0 {
			return 0
		}
		return float64(lat[int(p*float64(len(lat)-1))].Microseconds()) / 1000
	}
	res.LatencyMs = map[string]float64{"p50": pct(0.50), "p95": pct(0.95), "p99": pct(0.99), "max": pct(1)}
	res.Throughput = float64(res.Requests) / elapsed.Seconds()
	res.Config.Clients, res.Config.Duration, res.Config.Faults = *clients, dur.String(), *faults
	res.Config.Store = "memory"
	if *db != "" {
		res.Config.Store = "postgres"
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	if *out != "" {
		os.WriteFile(*out, b, 0o644) //nolint:errcheck
	}
	fmt.Println(string(b))
	if len(res.Check.Violations) > 0 {
		os.Exit(1)
	}
}
