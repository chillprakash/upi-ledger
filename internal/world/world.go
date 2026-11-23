// Package world assembles the whole system for running it for real (the
// server) and for load tests: banks, the switch, RBI and the app engine, on
// in-memory or Postgres ledgers, with background status checks, UDIR passes
// and settlement, and an end-of-run check of every invariant.
package world

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chillprakash/upi-ledger/internal/bank"
	"github.com/chillprakash/upi-ledger/internal/clock"
	"github.com/chillprakash/upi-ledger/internal/ledger"
	"github.com/chillprakash/upi-ledger/internal/money"
	"github.com/chillprakash/upi-ledger/internal/netsim"
	"github.com/chillprakash/upi-ledger/internal/npci"
	"github.com/chillprakash/upi-ledger/internal/psp"
	"github.com/chillprakash/upi-ledger/internal/recon"
)

// Config shapes a world.
type Config struct {
	DatabaseURL string // empty: in-memory ledgers
	Schema      string // schema prefix for this run (Postgres only)
	Banks       int
	Users       int // per bank
	Opening     money.Paise
	FaultRate   float64
	Speed       float64 // simulated seconds per real second (status checks at 60 s happen after 60/Speed s)
	Seed        uint64
}

// User is a customer with a UPI ID.
type User struct {
	VPA     string
	Bank    string
	Account ledger.AccountID
}

// World is a running system.
type World struct {
	Cfg    Config
	Clock  *clock.Sim
	Net    *netsim.Net
	Switch *npci.Switch
	RBI    *npci.RBI
	Banks  map[string]*bank.Bank
	App    *psp.Engine
	Users  []User

	random  *netsim.Random
	opening money.Paise
	pools   []*pgxpool.Pool
	admin   *pgxpool.Pool
	schemas []string
	mu      sync.Mutex // serialises background passes
}

// Build creates the world.
func Build(ctx context.Context, cfg Config) (*World, error) {
	if cfg.Banks == 0 {
		cfg.Banks = 3
	}
	if cfg.Users == 0 {
		cfg.Users = 50
	}
	if cfg.Opening == 0 {
		cfg.Opening = money.Rupees(100000)
	}
	if cfg.Speed == 0 {
		cfg.Speed = 1
	}
	w := &World{Cfg: cfg, Clock: clock.NewSim(time.Now().In(npci.IST)), Banks: map[string]*bank.Bank{}}
	w.random = netsim.NewRandom(rand.New(rand.NewPCG(cfg.Seed, 7)), cfg.FaultRate*0.25, cfg.FaultRate*0.35, cfg.FaultRate*0.2, cfg.FaultRate*0.2)
	w.Net = &netsim.Net{Plan: w.random} // real time: calls do not move the clock
	w.Switch = npci.New(w.Net, w.Clock)

	newLedger := func(name string) (ledger.Ledger, error) {
		if cfg.DatabaseURL == "" {
			return ledger.NewMemory(), nil
		}
		pool, err := w.schemaPool(ctx, cfg.Schema+"_"+name)
		if err != nil {
			return nil, err
		}
		return ledger.NewPostgres(ctx, pool)
	}
	rl, err := newLedger("rbi")
	if err != nil {
		return nil, err
	}
	w.RBI = &npci.RBI{Ledger: rl}
	for b := 0; b < cfg.Banks; b++ {
		name := fmt.Sprintf("bank%d", b)
		l, err := newLedger(name)
		if err != nil {
			return nil, err
		}
		bk, err := bank.New(ctx, name, l)
		if err != nil {
			return nil, err
		}
		bk.Now = w.Clock.Now
		w.Banks[name] = bk
		w.Switch.AddBank(bk)
		for u := 0; u < cfg.Users; u++ {
			acct := ledger.AccountID(fmt.Sprintf("acct-%d-%d", b, u))
			if err := bk.OpenAccount(ctx, acct, cfg.Opening); err != nil {
				return nil, err
			}
			w.opening += cfg.Opening
			vpa := fmt.Sprintf("user%d.%d@%s", b, u, name)
			w.Switch.Register(npci.Address{VPA: vpa, Bank: name, Account: acct, Name: fmt.Sprintf("USER %d %d", b, u)})
			w.Users = append(w.Users, User{vpa, name, acct})
		}
	}
	w.App = psp.New(w.Switch, w.Net, w.Clock, "WAP")
	if cfg.DatabaseURL != "" {
		pool, err := w.schemaPool(ctx, cfg.Schema+"_psp")
		if err != nil {
			return nil, err
		}
		st, err := psp.NewPGStore(ctx, pool)
		if err != nil {
			return nil, err
		}
		w.App.Store = st
	}
	return w, nil
}

func (w *World) schemaPool(ctx context.Context, schema string) (*pgxpool.Pool, error) {
	if w.admin == nil {
		a, err := pgxpool.New(ctx, w.Cfg.DatabaseURL)
		if err != nil {
			return nil, err
		}
		w.admin = a
	}
	if _, err := w.admin.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(w.Cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	w.pools = append(w.pools, pool)
	w.schemas = append(w.schemas, schema)
	return pool, nil
}

// Run drives the clock and background work until ctx ends.
func (w *World) Run(ctx context.Context) {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	last := time.Now()
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			w.Clock.Advance(time.Duration(float64(now.Sub(last)) * w.Cfg.Speed))
			last = now
			if n++; n%5 == 0 {
				w.pass(ctx)
			}
		}
	}
}

func (w *World) pass(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.App.Tick(ctx)
	w.Switch.Resolve(ctx)
	w.Switch.Settle(ctx, w.RBI, w.Clock.Now()) //nolint:errcheck
}

// Quiesce stops faults and moves simulated time on until every payment has
// settled: status checks, UDIR passes, all settlement cycles, reconciliation.
func (w *World) Quiesce(ctx context.Context) {
	w.random.Healed = true
	for i := 0; i < 400; i++ {
		w.Clock.Advance(30 * time.Second)
		w.pass(ctx)
	}
	for i := 0; i < 2*24*6; i++ {
		w.Clock.Advance(10 * time.Minute)
		w.pass(ctx)
	}
	w.App.Reconcile(w.Switch.Records())
}

// CheckReport is the end-of-run verdict.
type CheckReport struct {
	Payments         int            `json:"payments"`
	ByState          map[string]int `json:"by_state"`
	Transactions     int            `json:"switch_transactions"`
	SettlementFiles  int            `json:"settlement_files"`
	SettlementLines  int            `json:"settlement_lines"`
	ReconBreaks      map[string]int `json:"recon_breaks"`
	Faults           map[string]int `json:"faults_injected"`
	ReconCorrections map[string]int `json:"recon_corrections"`
	StatusChecks     int            `json:"status_checks"`
	Violations       []string       `json:"violations"`
}

// Check verifies every invariant against the stored ledgers.
func (w *World) Check(ctx context.Context) CheckReport {
	rep := CheckReport{ByState: map[string]int{}, Faults: map[string]int{}}
	bad := func(f string, a ...any) { rep.Violations = append(rep.Violations, fmt.Sprintf(f, a...)) }
	names := make([]string, 0, len(w.Banks))
	for n := range w.Banks {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := w.Banks[n].Ledger.CheckInvariants(ctx); err != nil {
			bad("%s: %v", n, err)
		}
		pool, _ := w.Banks[n].Ledger.Balance(ctx, bank.Pool)
		if pool.Posted() != -money.Paise(w.Cfg.Users)*w.Cfg.Opening {
			bad("%s pool %s after settlement", n, pool.Posted())
		}
	}
	if err := w.RBI.Ledger.CheckInvariants(ctx); err != nil {
		bad("rbi: %v", err)
	}
	var held money.Paise
	for _, u := range w.Users {
		b, _ := w.Banks[u.Bank].Ledger.Balance(ctx, u.Account)
		held += b.Posted()
	}
	if held != w.opening {
		bad("money not conserved: customers hold %s, opened %s", held, w.opening)
	}
	payments := w.App.Payments()
	rr, err := recon.Run(ctx, w.Switch, payments)
	if err != nil {
		bad("recon: %v", err)
	}
	rep.ReconBreaks = rr.ByCategory()
	for _, b := range rr.Breaks {
		bad("recon %s %s %s", b.Category, b.TxnID, b.Detail)
	}
	for _, p := range payments {
		rep.Payments++
		rep.ByState[string(p.State)]++
		if !p.State.Final() {
			bad("payment %s left %s", p.ID, p.State)
		}
	}
	rep.Transactions = len(w.Switch.Records())
	for _, f := range w.Switch.Files() {
		rep.SettlementFiles++
		rep.SettlementLines += len(f.Lines)
	}
	for k := netsim.ConnectFail; k <= netsim.Duplicate; k++ {
		rep.Faults[k.String()] = w.random.Counted[k]
	}
	st := w.App.Stats()
	rep.ReconCorrections, rep.StatusChecks = st.ReconCorrections, st.StatusChecks
	return rep
}

// Handler serves the payment API plus /metrics and /healthz.
func (w *World) Handler() http.Handler {
	mux := http.NewServeMux()
	api := w.App.Handler()
	mux.Handle("/payments", api)
	mux.Handle("/payments/", api)
	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, _ *http.Request) { rw.Write([]byte("ok\n")) }) //nolint:errcheck
	mux.HandleFunc("GET /metrics", func(rw http.ResponseWriter, _ *http.Request) {
		var b strings.Builder
		states := map[string]int{}
		for _, p := range w.App.Payments() {
			states[string(p.State)]++
		}
		keys := make([]string, 0, len(states))
		for k := range states {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("# HELP upi_payments Payments by state.\n# TYPE upi_payments gauge\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "upi_payments{state=%q} %d\n", k, states[k])
		}
		st := w.App.Stats()
		fmt.Fprintf(&b, "# TYPE upi_status_checks_total counter\nupi_status_checks_total %d\n", st.StatusChecks)
		fmt.Fprintf(&b, "# TYPE upi_recon_flagged_total counter\nupi_recon_flagged_total %d\n", st.ReconFlagged)
		fmt.Fprintf(&b, "# TYPE upi_store_write_errors_total counter\nupi_store_write_errors_total %d\n", st.PersistErrors)
		fmt.Fprintf(&b, "# TYPE upi_unsettled_success gauge\nupi_unsettled_success %d\n", len(w.Switch.Unsettled()))
		fmt.Fprintf(&b, "# TYPE upi_settlement_files_total counter\nupi_settlement_files_total %d\n", len(w.Switch.Files()))
		rw.Header().Set("Content-Type", "text/plain; version=0.0.4")
		rw.Write([]byte(b.String())) //nolint:errcheck
	})
	return mux
}

// Close releases database pools; drop removes this run's schemas.
func (w *World) Close(ctx context.Context, drop bool) {
	for _, p := range w.pools {
		p.Close()
	}
	if w.admin != nil {
		if drop {
			for _, s := range w.schemas {
				w.admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+s+" CASCADE") //nolint:errcheck
			}
		}
		w.admin.Close()
	}
}
