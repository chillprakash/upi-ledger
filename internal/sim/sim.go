// Package sim is deterministic simulation testing for the whole UPI path:
// app (PSP) -> NPCI switch -> remitter and beneficiary banks -> settlement
// at RBI, on one simulated clock and one seeded fault plan. A run generates
// users, merchants, money and payments (with duplicate submissions, reused
// keys, refunds), injects network faults, app crashes and multi-day bank
// outages, lets the system settle through settlement cycles and RBI
// compensation, and then checks invariants across the app's records, the
// switch's records and settlement files, the banks' ledgers and the RBI
// ledger. The same seed always replays the same run.
package sim

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/chillprakash/upi-ledger/internal/bank"
	"github.com/chillprakash/upi-ledger/internal/chat"
	"github.com/chillprakash/upi-ledger/internal/clock"
	"github.com/chillprakash/upi-ledger/internal/ledger"
	"github.com/chillprakash/upi-ledger/internal/money"
	"github.com/chillprakash/upi-ledger/internal/netsim"
	"github.com/chillprakash/upi-ledger/internal/npci"
	"github.com/chillprakash/upi-ledger/internal/psp"
	"github.com/chillprakash/upi-ledger/internal/rbi"
	"github.com/chillprakash/upi-ledger/internal/recon"
)

// Config shapes a run. Zero values get defaults.
type Config struct {
	Banks        int
	UsersPerBank int
	Payments     int
	// FaultRate is the share of calls that hit a network fault (default:
	// random per run, 5% to 30%).
	FaultRate float64
	// FrozenRate is the share of accounts that refuse credits (RET).
	FrozenRate float64
	// CrashRate is the chance the app crashes during a payment.
	CrashRate float64
	// OutageRate is the chance a run has a 30-60 hour bank outage.
	OutageRate float64
}

func (c *Config) defaults() {
	if c.Banks == 0 {
		c.Banks = 3
	}
	if c.UsersPerBank == 0 {
		c.UsersPerBank = 6
	}
	if c.Payments == 0 {
		c.Payments = 60
	}
	if c.FrozenRate == 0 {
		c.FrozenRate = 0.1
	}
	if c.CrashRate == 0 {
		c.CrashRate = 0.02
	}
	if c.OutageRate == 0 {
		c.OutageRate = 0.15
	}
}

// Report is what one run produced.
type Report struct {
	Seed              uint64
	Payments          int
	DuplicateSubmits  int
	KeyReuseRejected  int
	Crashes           int
	Recovered         int
	Outage            bool
	OutageCalls       int
	Refunds           int
	RefundsRejected   int
	LitePayments      int
	BlockDebits       int
	BlockDeclines     int
	ChatOrders        int
	ChatRetaps        int
	ByState           map[string]int
	BySwitchStatus    map[string]int
	Faults            map[string]int
	StatusChecks      int
	ReconFlagged      int
	ReconCorrections  map[string]int
	SettlementFiles   int
	SettlementLines   int
	CompensationPaid  money.Paise
	CompensatedTxns   int
	SimulatedDuration time.Duration
	Violations        []string
}

// OK reports whether every invariant held.
func (r Report) OK() bool { return len(r.Violations) == 0 }

type user struct {
	vpa      string
	bank     string
	acct     ledger.AccountID
	merchant bool
	lite     bool
}

// Run executes one seeded simulation.
func Run(seed uint64, cfg Config) Report {
	cfg.defaults()
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	start := time.Date(2026, 11, 23, 10, 0, 0, 0, npci.IST)
	clk := clock.NewSim(start)

	rate := cfg.FaultRate
	if rate == 0 {
		rate = 0.05 + rng.Float64()*0.25
	}
	random := netsim.NewRandom(rand.New(rand.NewPCG(rng.Uint64(), rng.Uint64())), rate*0.25, rate*0.35, rate*0.2, rate*0.2)
	outages := &netsim.Outages{Inner: random, Clock: clk, Down: map[string][]netsim.Window{}}
	net := &netsim.Net{Clock: clk, Plan: outages, Latency: 300 * time.Millisecond}
	sw := npci.New(net, clk)
	central := &npci.RBI{Ledger: ledger.NewMemory()}

	rep := Report{Seed: seed, ByState: map[string]int{}, BySwitchStatus: map[string]int{}, Faults: map[string]int{}}
	var users []user
	var opening money.Paise
	openingByBank := map[string]money.Paise{}
	for b := 0; b < cfg.Banks; b++ {
		name := fmt.Sprintf("bank%d", b)
		bk, err := bank.New(ctx, name, ledger.NewMemory())
		if err != nil {
			panic(err)
		}
		bk.Now = clk.Now
		sw.AddBank(bk)
		for u := 0; u < cfg.UsersPerBank; u++ {
			acct := ledger.AccountID(fmt.Sprintf("acct-%d-%d", b, u))
			amt := money.Paise(rng.IntN(5000) * 100)
			if err := bk.OpenAccount(ctx, acct, amt); err != nil {
				panic(err)
			}
			opening += amt
			openingByBank[name] += amt
			vpa := fmt.Sprintf("user%d.%d@%s", b, u, name)
			sw.Register(npci.Address{VPA: vpa, Bank: name, Account: acct, Name: fmt.Sprintf("USER %d %d", b, u)})
			if rng.Float64() < cfg.FrozenRate {
				bk.Freeze(acct)
			}
			usr := user{vpa: vpa, bank: name, acct: acct, merchant: rng.Float64() < 0.25}
			// Some users turn on UPI LITE and top it up from their account.
			if rng.Float64() < 0.4 && amt > 0 {
				_ = bk.EnableLite(ctx, acct, true, clk.Now())
				top := money.Paise(rng.Int64N(int64(min(amt, bank.LiteBalance))/100+1)) * 100
				if top > 0 && bk.LiteTopUp(ctx, "init-"+string(acct), acct, top, true, clk.Now()) == nil {
					usr.lite = true
				}
			}
			users = append(users, usr)
		}
	}
	paymentPhase := time.Duration(cfg.Payments) * 10 * time.Second
	var outageEnd time.Time
	if rng.Float64() < cfg.OutageRate {
		rep.Outage = true
		b := rng.IntN(cfg.Banks)
		from := start.Add(time.Duration(rng.Int64N(int64(paymentPhase) + 1)))
		outageEnd = from.Add(time.Duration(30+rng.IntN(31)) * time.Hour)
		outages.Down[fmt.Sprintf("npci>bank%d", b)] = []netsim.Window{{From: from, To: outageEnd}}
	}

	store := psp.NewMemStore()
	crashRng := rand.New(rand.NewPCG(rng.Uint64(), 1))
	var eng *psp.Engine
	newEngine := func() {
		eng = psp.New(sw, net, clk, "SIM")
		eng.Store = store
		eng.Crash = func(string) bool { return crashRng.Float64() < cfg.CrashRate }
		if err := eng.Restore(ctx); err != nil {
			panic(err)
		}
	}
	newEngine()
	var stats psp.Stats // accumulated across engine restarts
	stats.ReconCorrections = map[string]int{}
	collect := func() {
		s := eng.Stats()
		stats.StatusChecks += s.StatusChecks
		stats.ReconFlagged += s.ReconFlagged
		stats.Recovered += s.Recovered
		for k, v := range s.ReconCorrections {
			stats.ReconCorrections[k] += v
		}
	}

	type submitted struct {
		key string
		req psp.Request
	}
	var subs []submitted
	refundSeq := 0
	orderPay := map[string]string{} // order reference -> payment ID
	for i := 0; i < cfg.Payments; i++ {
		clk.Advance(time.Duration(rng.IntN(20_000)) * time.Millisecond)
		var key string
		var req psp.Request
		if len(subs) > 0 && rng.Float64() < 0.15 {
			s := subs[rng.IntN(len(subs))]
			key, req = s.key, s.req
			rep.DuplicateSubmits++
		} else {
			from, to := users[rng.IntN(len(users))], users[rng.IntN(len(users))]
			if from.vpa == to.vpa {
				continue
			}
			key = fmt.Sprintf("key-%d-%d", seed, i)
			req = psp.Request{Payer: from.vpa, Payee: to.vpa, Amount: money.Paise((1 + rng.IntN(3000)) * 100), Kind: psp.Push, Merchant: to.merchant}
			if from.lite && rng.Float64() < 0.6 {
				req.Amount = money.Paise((1 + rng.IntN(600)) * 100) // small payments, some above Rs 500
			}
			if from.lite {
				// The device's mirror of the LITE balance (simulator reads the bank's).
				bk := sw.Banks()[from.bank]
				lb, _ := bk.Ledger.Balance(ctx, bank.LiteAccount(from.acct))
				req.Lite = psp.ChooseLite(req.Amount, true, lb.Available(), true)
				if rng.Float64() < 0.05 {
					// A misbehaving client forces LITE for a large amount: the bank must refuse.
					req.Lite, req.Amount = true, money.Paise((1000+rng.IntN(2000))*100)
				}
				if req.Lite {
					rep.LitePayments++
				}
				if rng.Float64() < 0.1 {
					// A top-up mid-run, sometimes past the Rs 5,000 cap (must be refused).
					_ = bk.LiteTopUp(ctx, fmt.Sprintf("top-%d-%d", seed, i), from.acct, money.Paise((1+rng.IntN(6000))*100), true, clk.Now())
				}
			}
			// Payments to merchants sometimes come from a chat order.
			if to.merchant && !req.Lite && rng.Float64() < 0.5 {
				rep.ChatOrders++
				od := order(fmt.Sprintf("ORD-%d-%d", seed, i), req.Amount)
				taps := 1 + rng.IntN(3)
				for k := 0; k < taps; k++ {
					pm, err := chat.Checkout(ctx, eng, od, from.vpa, to.vpa, clk.Now())
					if err == nil {
						if prev, ok := orderPay[od.ReferenceID()]; ok && prev != pm.ID {
							rep.Violations = append(rep.Violations, fmt.Sprintf("order %s paid by %s and %s", od.ReferenceID(), prev, pm.ID))
						}
						orderPay[od.ReferenceID()] = pm.ID
					}
					if errors.Is(err, psp.ErrCrashed) {
						rep.Crashes++
						collect()
						newEngine()
					}
				}
				rep.ChatRetaps += taps - 1
				continue
			}
		}
		_, err := eng.Create(ctx, key, req)
		switch {
		case errors.Is(err, psp.ErrCrashed):
			rep.Crashes++
			subs = append(subs, submitted{key, req}) // the user will retry
			collect()
			newEngine()
		case err == nil:
			subs = append(subs, submitted{key, req})
		}
		if len(subs) > 0 && rng.Float64() < 0.03 {
			s := subs[rng.IntN(len(subs))]
			bad := s.req
			bad.Amount += 100
			if _, err := eng.Create(ctx, s.key, bad); errors.Is(err, psp.ErrIdempotencyMismatch) {
				rep.KeyReuseRejected++
			} else if !errors.Is(err, psp.ErrCrashed) {
				rep.Violations = append(rep.Violations, fmt.Sprintf("key reuse with a different body was not rejected: %v", err))
			}
		}
		// Merchants refund some successful payments, partially or fully,
		// sometimes asking twice or for too much.
		if rng.Float64() < 0.08 {
			var cands []npci.Record
			for _, r := range sw.Records() {
				if r.Kind == npci.KindPay && r.Merchant && r.Status == npci.Success {
					cands = append(cands, r)
				}
			}
			if len(cands) > 0 {
				o := cands[rng.IntN(len(cands))]
				refundSeq++
				amt := o.Amount
				if rng.Float64() < 0.5 {
					amt = money.Paise(1+rng.Int64N(int64(o.Amount)/100)) * 100
				}
				if rng.Float64() < 0.2 {
					amt = o.Amount + 100
				}
				id := fmt.Sprintf("RFD%d-%d", seed, refundSeq)
				for k := 0; k < 1+rng.IntN(2); k++ { // sometimes the merchant retries
					_, err := sw.Refund(ctx, npci.RefundRequest{RefundID: id, OrigTxn: o.TxnID, Amount: amt})
					if err == nil {
						rep.Refunds++
					} else {
						rep.RefundsRejected++
					}
				}
			}
		}
		// Devices sync their LITE state after payments, most of the time promptly.
		for _, u := range users {
			if u.lite && rng.Float64() < 0.7 {
				sw.Banks()[u.bank].LiteSync(u.acct)
			}
		}
		// Reserve Pay: a user blocks funds for a merchant; the merchant debits
		// against it, sometimes asking for more than is left.
		if rng.Float64() < 0.06 {
			from, to := users[rng.IntN(len(users))], users[rng.IntN(len(users))]
			if to.merchant && from.vpa != to.vpa {
				bk := sw.Banks()[from.bank]
				bal, _ := bk.Ledger.Balance(ctx, from.acct)
				if bal.Available() >= money.Rupees(10) {
					blk := fmt.Sprintf("BLK%d-%d", seed, i)
					amt := money.Paise(rng.Int64N(int64(bal.Available())/100)+1) * 100
					if bk.CreateBlock(ctx, blk, from.acct, to.vpa, amt, clk.Now().Add(time.Duration(1+rng.IntN(48))*time.Hour)) == nil {
						for d := 0; d < 1+rng.IntN(4); d++ {
							r, err := sw.Pay(ctx, npci.PayRequest{TxnID: fmt.Sprintf("%s-D%d", blk, d), Payer: from.vpa, Payee: to.vpa,
								Amount: money.Paise(rng.Int64N(int64(amt)/100)+1) * 100, Merchant: true, Block: blk})
							if err == nil && r.Debit == npci.LegOK {
								rep.BlockDebits++
							} else {
								rep.BlockDeclines++
							}
						}
					}
				}
			}
		}
		eng.Tick(ctx)
		if rng.Float64() < 0.3 {
			sw.Resolve(ctx)
		}
	}

	// Let the system settle. For the first hours the clock moves in 30 s
	// steps so status checks run on schedule; then in 10 min steps through
	// any outage and the following settlement and dispute cycles. Two hours
	// after the payment phase the random faults heal; outages end on their own.
	healAt := start.Add(paymentPhase + 2*time.Hour)
	endBy := start.Add(paymentPhase + 4*time.Hour)
	if outageEnd.After(endBy) {
		endBy = outageEnd.Add(3 * time.Hour)
	}
	lastComp := time.Time{}
	for clk.Now().Before(endBy.Add(24 * time.Hour)) {
		step := 30 * time.Second
		if clk.Now().After(start.Add(paymentPhase + 3*time.Hour)) {
			step = 10 * time.Minute
		}
		clk.Advance(step)
		if !clk.Now().Before(healAt) {
			random.Healed = true
		}
		eng.Tick(ctx)
		left := sw.Resolve(ctx)
		if _, err := sw.Settle(ctx, central, clk.Now()); err != nil {
			rep.Violations = append(rep.Violations, "settle: "+err.Error())
			break
		}
		if clk.Now().Sub(lastComp) >= time.Hour {
			lastComp = clk.Now()
			payCompensation(ctx, sw, clk.Now(), &rep)
		}
		if left == 0 && clk.Now().After(endBy) && len(sw.Unsettled()) == 0 {
			break
		}
	}
	payCompensation(ctx, sw, clk.Now(), &rep)
	// Before end-of-day reconciliation papers over anything: no payment may
	// still be "initiated" (a crash left it, and restart must have picked it up).
	for _, p := range eng.Payments() {
		if p.State == psp.Initiated {
			rep.Violations = append(rep.Violations, fmt.Sprintf("payment %s still initiated hours later: not recovered after a crash", p.ID))
		}
	}
	eng.Reconcile(sw.Records())
	collect()

	rep.SimulatedDuration = clk.Now().Sub(start)
	for k := netsim.ConnectFail; k <= netsim.Duplicate; k++ {
		rep.Faults[k.String()] = random.Counted[k]
	}
	rep.OutageCalls = outages.Hits
	rep.StatusChecks, rep.ReconFlagged, rep.Recovered, rep.ReconCorrections = stats.StatusChecks, stats.ReconFlagged, stats.Recovered, stats.ReconCorrections
	for _, f := range sw.Files() {
		rep.SettlementFiles++
		rep.SettlementLines += len(f.Lines)
	}
	rep.Violations = append(rep.Violations, check(ctx, sw, central, eng, users, opening, openingByBank, clk.Now(), rep.CompensationPaid)...)
	for _, r := range sw.Records() {
		if owed, _ := rbi.Owed(ctx, r, sw.Banks()[r.Payer.Bank], clk.Now()); owed > 0 {
			rep.CompensatedTxns++
		}
	}
	for _, p := range eng.Payments() {
		rep.Payments++
		rep.ByState[string(p.State)]++
	}
	for _, r := range sw.Records() {
		rep.BySwitchStatus[string(r.Kind)+":"+string(r.Status)]++
	}
	return rep
}

func payCompensation(ctx context.Context, sw *npci.Switch, now time.Time, rep *Report) {
	banks := sw.Banks()
	for _, r := range sw.Records() {
		paid, err := rbi.Pay(ctx, r, banks[r.Payer.Bank], now)
		if err != nil {
			rep.Violations = append(rep.Violations, "compensation: "+err.Error())
			continue
		}
		rep.CompensationPaid += paid
	}
}

// check verifies the invariants after the system has settled.
func check(ctx context.Context, sw *npci.Switch, central *npci.RBI, eng *psp.Engine, users []user,
	opening money.Paise, openingByBank map[string]money.Paise, now time.Time, compPaid money.Paise) []string {
	var v []string
	bad := func(format string, a ...any) { v = append(v, fmt.Sprintf(format, a...)) }
	banks := sw.Banks()
	names := make([]string, 0, len(banks))
	for n := range banks {
		names = append(names, n)
	}
	sort.Strings(names)

	// Every ledger replays from its log to the same balances and sums to zero.
	for _, n := range names {
		if err := banks[n].Ledger.CheckInvariants(ctx); err != nil {
			bad("%s: %v", n, err)
		}
	}
	if err := central.Ledger.CheckInvariants(ctx); err != nil {
		bad("rbi: %v", err)
	}

	// Conservation: customers hold exactly what they opened with plus RBI
	// compensation. Payments and refunds only move money between them.
	var held money.Paise
	for _, u := range users {
		b, err := banks[u.bank].Ledger.Balance(ctx, u.acct)
		if err != nil {
			bad("balance %s: %v", u.acct, err)
		}
		held += b.Posted()
		if u.lite {
			lb, err := banks[u.bank].Ledger.Balance(ctx, bank.LiteAccount(u.acct))
			if err != nil {
				bad("lite balance %s: %v", u.acct, err)
			}
			if lb.Posted() > bank.LiteBalance {
				bad("%s LITE balance %s above the Rs 5,000 limit", u.acct, lb.Posted())
			}
			held += lb.Posted()
		}
	}
	if held != opening+compPaid {
		bad("money not conserved: customers hold %s, opened %s + compensation %s", held, opening, compPaid)
	}

	// After every cycle has settled, each bank's UPI pool holds only the
	// opening balances it funded: all interbank flows went through RBI.
	for _, n := range names {
		pool, _ := banks[n].Ledger.Balance(ctx, bank.Pool)
		if pool.Posted() != -openingByBank[n] {
			bad("%s pool %s after settlement, want %s", n, pool.Posted(), -openingByBank[n])
		}
	}

	// LITE payments never exceed Rs 1,000; Reserve Pay debits never exceed their block.
	byBlock := map[string]money.Paise{}
	for _, r := range sw.Records() {
		if r.Lite && r.Status == npci.Success && r.Amount > bank.LitePerTxn {
			bad("%s LITE payment of %s succeeded", r.TxnID, r.Amount)
		}
		if r.Block != "" && r.Status == npci.Success {
			byBlock[r.Payer.Bank+"/"+r.Block] += r.Amount
		}
	}
	for k, used := range byBlock {
		bk, id, _ := strings.Cut(k, "/")
		if blk, ok := banks[bk].GetBlock(id); !ok || used > blk.Amount {
			bad("block %s debited %s of %s", id, used, blk.Amount)
		}
	}

	// Compensation goes to bank accounts, never to a LITE wallet.
	for _, n := range names {
		ts, _ := banks[n].Ledger.Transfers(ctx)
		for _, t := range ts {
			if strings.HasPrefix(t.ID, "comp:") && strings.HasPrefix(string(t.Credit), "lite:") {
				bad("%s compensation credited to LITE wallet %s", t.ID, t.Credit)
			}
		}
	}

	// Refunds never exceed the original payment.
	refunded := map[string]money.Paise{}
	records := map[string]npci.Record{}
	for _, r := range sw.Records() {
		records[r.TxnID] = r
		if r.Kind == npci.KindRefund && r.Status == npci.Success {
			refunded[r.OrigTxn] += r.Amount
		}
	}
	for orig, amt := range refunded {
		if amt > records[orig].Amount {
			bad("%s refunded %s of %s", orig, amt, records[orig].Amount)
		}
	}

	// Independent oracle for RBI compensation, written from the rule, not
	// from the rbi package: days the payer's money spent in limbo after the
	// deadline day, where limbo ends at the successful credit or the reversal.
	for _, r := range records {
		if r.Kind != npci.KindPay {
			continue
		}
		debited, _ := banks[r.Payer.Bank].Moved(ctx, r.TxnID, "debit")
		end := r.ReversedAt
		if r.Status == npci.Success {
			end = r.FinalAt
		}
		if end.IsZero() {
			end = now
		}
		allowed := 1
		if r.Merchant {
			allowed = 5
		}
		c, e := r.CreatedAt.In(npci.IST), end.In(npci.IST)
		cDay := time.Date(c.Year(), c.Month(), c.Day(), 0, 0, 0, 0, npci.IST)
		eDay := time.Date(e.Year(), e.Month(), e.Day(), 0, 0, 0, 0, npci.IST)
		late := int(eDay.Sub(cDay).Hours()/24) - allowed
		if late < 0 || !debited {
			late = 0
		}
		var paid int
		for d := 1; ; d++ {
			if _, ok, _ := banks[r.Payer.Bank].Ledger.Get(ctx, fmt.Sprintf("comp:%s:%d", r.TxnID, d)); !ok {
				break
			}
			paid++
		}
		if paid != late {
			bad("%s compensated %d days, rule says %d", r.TxnID, paid, late)
		}
	}

	// RBI compensation paid equals what the rules say is owed.
	for _, r := range records {
		owed, err := rbi.Owed(ctx, r, banks[r.Payer.Bank], now)
		if err != nil {
			bad("owed: %v", err)
			continue
		}
		var paid money.Paise
		for d := 1; ; d++ {
			_, ok, _ := banks[r.Payer.Bank].Ledger.Get(ctx, fmt.Sprintf("comp:%s:%d", r.TxnID, d))
			if !ok {
				break
			}
			paid += rbi.PerDay
		}
		if paid != owed {
			bad("%s compensation paid %s, owed %s", r.TxnID, paid, owed)
		}
	}

	// Three-way reconciliation must find no breaks.
	payments := eng.Payments()
	rr, err := recon.Run(ctx, sw, payments)
	if err != nil {
		bad("recon: %v", err)
	}
	for _, b := range rr.Breaks {
		bad("recon %s %s %s", b.Category, b.TxnID, b.Detail)
	}

	// App-side rules.
	seenTxn := map[string]string{}
	for _, p := range payments {
		if prev, dup := seenTxn[p.TxnID]; dup {
			bad("payments %s and %s share txn %s", prev, p.ID, p.TxnID)
		}
		seenTxn[p.TxnID] = p.ID
		if !p.State.Final() {
			bad("payment %s left %s", p.ID, p.State)
		}
		if p.Checks > len(psp.CheckOffsets) {
			bad("payment %s made %d status checks (max %d)", p.ID, p.Checks, len(psp.CheckOffsets))
		}
	}
	return v
}

// order builds a chat order_details message for amt and parses it the way a
// business's message would be.
func order(ref string, amt money.Paise) chat.OrderDetails {
	tax := amt / 10
	raw := fmt.Sprintf(`{"interactive":{"type":"order_details","action":{"name":"review_and_pay","parameters":{
		"reference_id":%q,"type":"physical-goods","currency":"INR","total_amount":{"value":%d,"offset":100},
		"order":{"status":"pending","items":[{"retailer_id":"sku","name":"item","amount":{"value":%d,"offset":100},"quantity":1}],
		"subtotal":{"value":%d,"offset":100},"tax":{"value":%d,"offset":100}}}}}}`, ref, amt, amt-tax, amt-tax, tax)
	od, err := chat.Parse([]byte(raw))
	if err != nil {
		panic(err)
	}
	return od
}
