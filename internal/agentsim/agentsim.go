// Package agentsim is deterministic simulation for agent commerce: users
// with wallets, two merchants, a PSP issuing UPI allowances, and thousands
// of agent sessions driven by honest and adversarial policies, under network
// faults. Afterwards it checks that no agent ever moved money the user did
// not approve, that every order matches exactly one payment, and that money
// is conserved and reconciles. The same seed replays the same run.
package agentsim

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/chillprakash/upi-ledger/internal/acp"
	"github.com/chillprakash/upi-ledger/internal/agent"
	"github.com/chillprakash/upi-ledger/internal/bank"
	"github.com/chillprakash/upi-ledger/internal/clock"
	"github.com/chillprakash/upi-ledger/internal/ledger"
	"github.com/chillprakash/upi-ledger/internal/money"
	"github.com/chillprakash/upi-ledger/internal/netsim"
	"github.com/chillprakash/upi-ledger/internal/npci"
	"github.com/chillprakash/upi-ledger/internal/recon"
)

// Policies an agent session may follow.
var Policies = []string{"honest", "overspend", "wrong_merchant", "replay", "after_revoke", "after_expiry", "forge", "inflate", "over_budget", "double_complete"}

// Report is one run's outcome.
type Report struct {
	Seed       uint64
	Sessions   map[string]int // by policy
	Paid       map[string]int // successful payments by policy
	Refused    map[string]int // policy attempts the rails refused
	Faults     map[string]int
	Orders     int
	Violations []string
}

// OK reports whether every invariant held.
func (r Report) OK() bool { return len(r.Violations) == 0 }

type user struct {
	wallet *agent.Wallet
	bank   string
	acct   ledger.AccountID
}

// Run executes one seeded agent-commerce simulation with n sessions.
func Run(seed uint64, n int) Report {
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(seed, seed*31+7))
	clk := clock.NewSim(time.Date(2026, 11, 23, 11, 0, 0, 0, npci.IST))
	rate := 0.05 + rng.Float64()*0.2
	plan := netsim.NewRandom(rand.New(rand.NewPCG(rng.Uint64(), 3)), rate*0.25, rate*0.35, rate*0.2, rate*0.2)
	net := &netsim.Net{Clock: clk, Plan: plan, Latency: 200 * time.Millisecond}
	sw := npci.New(net, clk)
	rep := Report{Seed: seed, Sessions: map[string]int{}, Paid: map[string]int{}, Refused: map[string]int{}, Faults: map[string]int{}}

	banks := map[string]*bank.Bank{}
	for _, name := range []string{"bank0", "bank1"} {
		b, _ := bank.New(ctx, name, ledger.NewMemory())
		b.Now = clk.Now
		banks[name] = b
		sw.AddBank(b)
	}
	var opening money.Paise
	open := func(b, acct, vpa string, amt money.Paise) {
		banks[b].OpenAccount(ctx, ledger.AccountID(acct), amt) //nolint:errcheck
		sw.Register(npci.Address{VPA: vpa, Bank: b, Account: ledger.AccountID(acct), Name: strings.ToUpper(acct)})
		opening += amt
	}
	open("bank1", "shop-a", "shopa@bank1", 0)
	open("bank0", "shop-b", "shopb@bank0", 0)
	keys := acp.NewKeyring()
	psp := acp.NewPSP(sw, banks, clk, keys, map[string]string{"shopa": "shopa@bank1", "shopb": "shopb@bank0"})
	tn := 0
	psp.IDs = func() string { tn++; return fmt.Sprintf("vt_upi_%06d", tn) }
	catalog := []acp.Product{{ID: "tea", Name: "Tea", UnitAmount: 25000}, {ID: "rice", Name: "Rice", UnitAmount: 60000}, {ID: "oil", Name: "Oil", UnitAmount: 18000}}
	shops := map[string]agent.Shop{
		"shopa": agent.LocalShop{M: acp.NewMerchant("shopa", "shopa@bank1", catalog, psp, clk)},
		"shopb": agent.LocalShop{M: acp.NewMerchant("shopb", "shopb@bank0", catalog, psp, clk)},
	}
	var users []user
	for i := 0; i < 6; i++ {
		b := []string{"bank0", "bank1"}[i%2]
		acct := fmt.Sprintf("u%d", i)
		open(b, acct, acct+"@"+b, money.Paise(rng.IntN(5000)+500)*100)
		seedKey := make([]byte, ed25519.SeedSize)
		seedKey[0], seedKey[1] = byte(seed), byte(i)
		priv := ed25519.NewKeyFromSeed(seedKey)
		keys.Register(acct+"-phone", acct+"@"+b, priv.Public().(ed25519.PublicKey))
		users = append(users, user{&agent.Wallet{VPA: acct + "@" + b, KeyID: acct + "-phone", Priv: priv, Budget: money.Paise(rng.IntN(2500)+300) * 100,
			Trust: map[string]bool{"shopa": true, "shopb": rng.Float64() < 0.8}, Clock: clk}, b, ledger.AccountID(acct)})
	}
	// An attacker's own key, registered to nobody's VPA.
	attackerSeed := make([]byte, ed25519.SeedSize)
	attackerSeed[0], attackerSeed[31] = byte(seed), 0xAA
	attacker := ed25519.NewKeyFromSeed(attackerSeed)
	approvedBy := map[string]money.Paise{} // token -> what the user's wallet signed
	policyOf := map[string]string{}        // token -> policy
	sessionPolicy := map[string]string{}   // session -> policy

	for i := 0; i < n; i++ {
		u := users[rng.IntN(len(users))]
		pol := Policies[rng.IntN(len(Policies))]
		if rng.Float64() < 0.4 {
			pol = "honest"
		}
		rep.Sessions[pol]++
		env := &agent.Env{Ctx: ctx, Wallet: u.wallet, Shops: shops, Delegator: agent.LocalDelegator{P: psp}}
		mid := []string{"shopa", "shopb"}[rng.IntN(2)]
		items := randomItems(rng)
		sess := createSession(env, mid, items)
		if sess.ID == "" {
			continue
		}
		sessionPolicy[sess.ID] = pol
		total := totalOf(sess)
		approve := func(session, merchant string, max money.Paise) string {
			out := env.Exec("request_payment_approval", mustJSON(map[string]any{"merchant_id": merchant, "session_id": session, "max_amount": max}))
			var r struct {
				Approved bool   `json:"approved"`
				Token    string `json:"token"`
			}
			json.Unmarshal([]byte(out), &r) //nolint:errcheck
			if r.Approved {
				approvedBy[r.Token], policyOf[r.Token] = max, pol
			}
			return r.Token
		}
		completeWith := func(merchant, session, token string) acp.Session {
			out := env.Exec("complete_checkout", mustJSON(map[string]any{"merchant_id": merchant, "session_id": session, "token": token}))
			var s acp.Session
			json.Unmarshal([]byte(out), &s) //nolint:errcheck
			return s
		}
		refusedIf := func(cond bool) {
			if cond {
				rep.Refused[pol]++
			}
		}
		switch pol {
		case "honest":
			if tok := approve(sess.ID, mid, total); tok != "" {
				completeWith(mid, sess.ID, tok)
			}
		case "overspend": // approved for the cart, then grows the cart
			if tok := approve(sess.ID, mid, total); tok != "" {
				env.Exec("update_checkout", mustJSON(map[string]any{"merchant_id": mid, "session_id": sess.ID, "items": append(items, acp.Item{ID: "rice", Quantity: 3})}))
				refusedIf(completeWith(mid, sess.ID, tok).Status != "completed")
			}
		case "wrong_merchant": // token for one shop used at the other
			other := map[string]string{"shopa": "shopb", "shopb": "shopa"}[mid]
			if tok := approve(sess.ID, mid, total); tok != "" {
				os := createSession(env, other, items)
				sessionPolicy[os.ID] = pol
				refusedIf(completeWith(other, os.ID, tok).Status != "completed")
			}
		case "replay": // pays once honestly, then reuses the token on a new cart
			if tok := approve(sess.ID, mid, total); tok != "" {
				completeWith(mid, sess.ID, tok)
				ns := createSession(env, mid, items)
				sessionPolicy[ns.ID] = pol
				refusedIf(completeWith(mid, ns.ID, tok).Status != "completed")
			}
		case "after_revoke":
			if tok := approve(sess.ID, mid, total); tok != "" {
				psp.Revoke(ctx, tok, u.wallet.VPA) //nolint:errcheck
				refusedIf(completeWith(mid, sess.ID, tok).Status != "completed")
			}
		case "after_expiry":
			if tok := approve(sess.ID, mid, total); tok != "" {
				clk.Advance(61 * time.Minute)
				refusedIf(completeWith(mid, sess.ID, tok).Status != "completed")
			}
		case "forge": // signs the "user's" approval with the attacker's own key
			m := acp.Mandate{PayerVPA: u.wallet.VPA, MerchantID: mid, MaxAmount: int64(total), Currency: "inr", CheckoutSessionID: sess.ID,
				ExpiresAt: clk.Now().Add(time.Hour).UTC().Format(time.RFC3339), Nonce: fmt.Sprintf("forge-%d-%d", seed, i)}
			keys.Register(fmt.Sprintf("attacker-%d", i), "attacker@nowhere", attacker.Public().(ed25519.PublicKey))
			sm, _ := acp.SignMandate(m, fmt.Sprintf("attacker-%d", i), attacker)
			_, err := delegateRaw(psp, u.wallet.VPA, sm, int64(total))
			refusedIf(err != nil)
		case "inflate": // the wallet signs X; the platform asks the PSP for 10X
			sm, err := u.wallet.Approve(mid, sess.ID, minP(total, u.wallet.Budget))
			if err == nil {
				_, err = delegateRaw(psp, u.wallet.VPA, sm, sm.Mandate.MaxAmount*10)
				refusedIf(err != nil)
			}
		case "over_budget": // asks for more than the user's budget, then tries anyway
			tok := approve(sess.ID, mid, u.wallet.Budget*2)
			refusedIf(completeWith(mid, sess.ID, tok).Status != "completed")
		case "double_complete":
			if tok := approve(sess.ID, mid, total); tok != "" {
				completeWith(mid, sess.ID, tok)
				completeWith(mid, sess.ID, tok)
			}
		}
		clk.Advance(time.Duration(rng.IntN(30)) * time.Second)
		if rng.Float64() < 0.3 {
			sw.Resolve(ctx)
			psp.Sweep(ctx)
			for _, s := range shops {
				s.(agent.LocalShop).M.Reconcile(ctx)
			}
		}
	}

	// Settle: faults heal, uncertain payments resolve, allowances expire,
	// orders reconcile, banks settle.
	plan.Healed = true
	for step := 0; step < 300; step++ {
		clk.Advance(10 * time.Minute)
		sw.Resolve(ctx)
		psp.Sweep(ctx)
		for _, s := range shops {
			s.(agent.LocalShop).M.Reconcile(ctx)
		}
	}
	if _, err := sw.Settle(ctx, &npci.RBI{Ledger: ledger.NewMemory()}, clk.Now().Add(48*time.Hour)); err != nil {
		rep.Violations = append(rep.Violations, "settle: "+err.Error())
	}
	for k := netsim.ConnectFail; k <= netsim.Duplicate; k++ {
		rep.Faults[k.String()] = plan.Counted[k]
	}
	rep.Violations = append(rep.Violations, check(ctx, sw, psp, shops, users, opening, approvedBy, policyOf, sessionPolicy, &rep)...)
	return rep
}

func check(ctx context.Context, sw *npci.Switch, psp *acp.PSP, shops map[string]agent.Shop, users []user, opening money.Paise,
	approvedBy map[string]money.Paise, policyOf, sessionPolicy map[string]string, rep *Report) []string {
	var v []string
	bad := func(f string, a ...any) { v = append(v, fmt.Sprintf(f, a...)) }
	tokens := map[string]acp.Token{}
	for _, t := range psp.Tokens() {
		tokens[t.ID] = t
	}
	paidPerToken := map[string]int{}
	paidAmount := map[string]money.Paise{} // txn -> amount
	for _, r := range sw.Records() {
		if !strings.HasPrefix(r.TxnID, "ACP") {
			continue
		}
		t, ok := tokens[strings.TrimPrefix(r.TxnID, "ACP")]
		if !ok {
			bad("%s: agent payment without a token", r.TxnID)
			continue
		}
		if r.Status != npci.Success {
			continue
		}
		paidPerToken[t.ID]++
		paidAmount[r.TxnID] = r.Amount
		rep.Paid[policyOf[t.ID]]++
		// The money may move only as the user's wallet signed it.
		signed, okSigned := approvedBy[t.ID]
		switch {
		case !okSigned:
			bad("%s: paid on token %s the user's wallet never approved", r.TxnID, t.ID)
		case r.Amount > signed:
			bad("%s: paid %s, user approved %s", r.TxnID, r.Amount, signed)
		case r.Payee.VPA != t.Merchant || r.Payer.VPA != t.PayerVPA:
			bad("%s: paid %s -> %s, token is %s -> %s", r.TxnID, r.Payer.VPA, r.Payee.VPA, t.PayerVPA, t.Merchant)
		}
	}
	for id, n := range paidPerToken {
		if n > 1 {
			bad("token %s paid %d times", id, n)
		}
	}
	// Orders and payments match one to one.
	orders := 0
	for mid, s := range shops {
		m := s.(agent.LocalShop).M
		for _, sess := range m.Sessions() {
			switch sess.Status {
			case "in_progress":
				bad("%s/%s left in progress", mid, sess.ID)
			case "completed":
				orders++
				tok := m.TokenOf(sess.ID)
				t := tokens[tok]
				if paidPerToken[tok] != 1 || t.Session != sess.ID || t.MerchantID != mid {
					bad("%s order %s without exactly one matching payment (token %s, paid %d)", mid, sess.ID, tok, paidPerToken[tok])
				} else if paidAmount["ACP"+tok] != money.Paise(total(sess)) {
					bad("%s order %s total %d, paid %s", mid, sess.ID, total(sess), paidAmount["ACP"+tok])
				}
				if p := sessionPolicy[sess.ID]; p != "honest" && p != "double_complete" && p != "replay" {
					// replay's first session and double_complete are honest payments
					bad("%s order %s completed by adversarial policy %s", mid, sess.ID, p)
				}
			}
		}
		for _, e := range m.Events() {
			_ = e
		}
	}
	rep.Orders = orders
	paidTotal := 0
	for _, n := range paidPerToken {
		paidTotal += n
	}
	if paidTotal != orders {
		bad("%d agent payments but %d orders", paidTotal, orders)
	}
	// No reservation is left behind.
	for _, t := range tokens {
		if b, ok := sw.Banks()[t.Bank].GetBlock(t.Block); ok && b.Status == "active" {
			bad("token %s still holds an active block", t.ID)
		}
	}
	// Ledgers, conservation, reconciliation.
	var held money.Paise
	banks := sw.Banks()
	names := make([]string, 0, len(banks))
	for n := range banks {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := banks[n].Ledger.CheckInvariants(ctx); err != nil {
			bad("%s: %v", n, err)
		}
	}
	for _, u := range users {
		b, _ := banks[u.bank].Ledger.Balance(ctx, u.acct)
		held += b.Posted()
		if b.DebitsPending != 0 {
			bad("%s still has %s reserved", u.acct, b.DebitsPending)
		}
	}
	for _, s := range []struct{ b, a string }{{"bank1", "shop-a"}, {"bank0", "shop-b"}} {
		b, _ := banks[s.b].Ledger.Balance(ctx, ledger.AccountID(s.a))
		held += b.Posted()
	}
	if held != opening {
		bad("money not conserved: %s held, %s opened", held, opening)
	}
	rr, err := recon.Run(ctx, sw, nil)
	if err != nil {
		bad("recon: %v", err)
	}
	for _, b := range rr.Breaks {
		bad("recon %s %s %s", b.Category, b.TxnID, b.Detail)
	}
	return v
}

func delegateRaw(psp *acp.PSP, vpa string, sm acp.SignedMandate, max int64) (string, error) {
	var req acp.DelegateRequest
	req.PaymentMethod.Type, req.PaymentMethod.VPA = acp.PaymentMethodType, vpa
	m := sm.Mandate
	req.Allowance = acp.Allowance{Reason: "one_time", MaxAmount: max, Currency: "inr", CheckoutSessionID: m.CheckoutSessionID, MerchantID: m.MerchantID, ExpiresAt: m.ExpiresAt}
	req.Metadata = map[string]string{}
	req.UserMandate = &sm
	return agent.LocalDelegator{P: psp}.Delegate(req)
}

func createSession(env *agent.Env, mid string, items []acp.Item) acp.Session {
	out := env.Exec("create_checkout", mustJSON(map[string]any{"merchant_id": mid, "items": items}))
	var s acp.Session
	json.Unmarshal([]byte(out), &s) //nolint:errcheck
	return s
}

func randomItems(rng *rand.Rand) []acp.Item {
	ids := []string{"tea", "rice", "oil"}
	n := 1 + rng.IntN(2)
	var out []acp.Item
	for i := 0; i < n; i++ {
		out = append(out, acp.Item{ID: ids[(i+rng.IntN(3))%3], Quantity: 1 + rng.IntN(2)})
	}
	return out
}

func totalOf(s acp.Session) money.Paise { return money.Paise(total(s)) }

func total(s acp.Session) int64 {
	for _, t := range s.Totals {
		if t.Type == "total" {
			return t.Amount
		}
	}
	return 0
}

func minP(a, b money.Paise) money.Paise {
	if a < b {
		return a
	}
	return b
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
