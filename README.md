# upi-ledger

A UPI-style payments engine in Go and Postgres, built to show one thing:
**no rupee is lost or doubled, whatever the network, the database or the
process does.**

It runs the whole UPI path on one machine: the app (the payer PSP, the seat
a messaging app like WhatsApp sits in), the NPCI switch, the remitter and
beneficiary banks, and settlement at RBI. It follows NPCI's UPI
Consolidated Circular of 18 Sep 2026, clause by clause. Every run ends by
checking that four independent records agree: the app's payment states,
the switch's transaction records and settlement files, each bank's
double-entry ledger, and the RBI settlement ledger.

```
 user --> app (PSP) --ReqPay--> NPCI switch --debit----> remitter bank  (ledger)
          | idempotency keys      | 15 s / 10 s deadlines  --credit---> beneficiary bank (ledger)
          | 60 s / max 3 checks   | deemed, reversal, RET       |
          | crash recovery        | UDIR resolution             |
          v                       v                             v
     payment state only     switch records -----> 12 settlement cycles --> RBI ledger (MNSB)
     (never balances)             |                             |
                                  +--------> reconciliation <---+
                                             RBI T+1 / T+5 compensation
```

## Proof

Two kinds of evidence, because each finds what the other cannot.

**Deterministic simulation** (seeded clock, network, crashes and outages; any
failing seed replays exactly):

```
$ go run ./cmd/upi-sim -seeds 100000
runs 100000  payments 4201288  wall 59.5s  simulated 1,284,067 hours
faults injected   connect_fail 788388  lost_response 1104801  slow 631017  duplicate 629710
app crashes 166166  recovered mid-flight 166166
bank outages (30-60 h) in 15004 runs
refunds 303326 ok, 184687 refused  settlement files 211467 (2167294 lines)
RBI compensation Rs 2776700 to 27767 payers
UPI LITE payments 908820  Reserve Pay debits 92272 (67739 refused)  chat orders 490127 (490466 extra taps)
invariant violations: 0 runs
```

**Load on real Postgres** (HTTP API, 32 concurrent clients, 20% duplicate
submissions, 10% of network calls failing, then settle and check everything
stored in Postgres):

```
$ go run ./cmd/upi-load -db postgres://... -clients 32 -duration 30s -faults 0.1
requests 148942 (4965/s)  duplicates 29908  latency p50 6.7 ms  p99 20.9 ms
payments 109225  faults injected 45936  reconciliation breaks 0  violations 0
```

### What every run checks

- Every ledger (each bank, RBI) replays from its transfer log to the same
  balances and sums to zero; no customer account below zero.
- Customers hold exactly what they opened with plus RBI compensation.
- After settlement, each bank's UPI pool is back to its opening position:
  all interbank money moved through RBI, and each cycle's clearing account
  is zero.
- Three-way reconciliation (banks' ledgers, switch records and settlement
  files, app states) finds no break: nothing settled without its debit or
  credit, nothing settled twice, no credit on a failed payment, no
  unreversed debit, no reversal without a debit.
- Refunds never exceed the payment; UPI LITE never above Rs 1,000 a payment
  or Rs 5,000 a balance; Reserve Pay never beyond its block.
- RBI compensation matches an independent restatement of the rule.
- No payment left uncertain; no transaction ID minted twice; at most 3
  status checks; an order paid at most once however often it is tapped;
  every crash recovered before end-of-day reconciliation.

### The checks catch real bugs

19 bugs were planted one at a time (for example: reversing a debit that
never happened, settling a line twice, ignoring a late credit when paying
compensation, forgetting to release a Reserve Pay reservation, tying a chat
order's key to the clock). The simulator caught every one.

It also found bugs I had written:

| Found by | Bug | Fix |
|---|---|---|
| Postgres load test | A cancelled database call was recorded as the bank's final answer, so a reversal stayed failed forever and money was not conserved | Banks never record transient errors as decisions; the switch treats them as timeouts; money movement ignores caller cancellation |
| Simulation | Compensation was paid on a "deemed" payment that later succeeded | Compensation counts days the payer's money spent in limbo, ending at the late credit or the reversal |
| 100,000-run sweep (3 runs) | A merchant refund of a UPI LITE payment landed in the LITE wallet and broke the Rs 5,000 cap | Refunds go to the bank account; a reversal that would break the cap goes there too |

And one finding about the rules themselves: **clause 12.5 read literally**
(a connection error on a status check means "treat as failed") marked
33,798 payments in 100,000 runs as failed that had in fact succeeded.
Reconciliation corrected every one. The rule is only safe with
reconciliation behind it.

## NPCI and RBI rules in code

Clauses are from NPCI's UPI Consolidated Circular, 18 Sep 2026, unless
marked RBI.

| Clause | Rule | Where | Test |
|---|---|---|---|
| 2.10, 2.11 | TPAP over 5% share: 2-10 partner banks; user's own bank's handle, else even spread; monthly report | `psp.Handles` | `TestHandles` |
| 2.13 | Payee's bank (CBS) name shown before authorisation | `psp.Create` | `TestSuccessShowsPayeeCBSName` |
| 2.16 | No collect for person-to-person | `psp.Create` | `TestP2PCollectRejected` |
| 12.1 | Pay legs 15 s; status, reversal, validate address 10 s | `npci` | `TestDebitTimeoutIsReversed`, `TestDeemedResolvesToSuccess` |
| 12.4 | U48 or checks exhausted: wait for settlement | `psp.check`, `psp.Reconcile` | `TestAtMostThreeChecks`, `TestCrashBeforeSendFailsCleanly` |
| 12.5, Annex C | Annexure C error: no more checks, treat as failed | `psp` | `TestConnectFailureFailsWithoutChecks`, `TestAnnexureCOnCheckNeedsReconciliation` |
| 12.8 | Status check after 60 s, max 3, within 2 h | `psp.check` | `TestFirstCheckAfter60s`, `TestAtMostThreeChecks` |
| 12.8 | Balance enquiry 50/day, list accounts 25/day, rolling from first request; app never stores balances | `psp.Limiter`; `psp` keeps state only | `TestLimiter` |
| 12.9.2 | Restrict system-initiated APIs in peak hours | `psp.IsPeak`, `psp.NextOffPeak` | `TestPeakHours` |
| 16 | Reserve Pay: block in own account, one per merchant, debits within the unused block, P2M only, user notified | `bank` (reserve.go), `npci` | `TestReservePay` |
| 18 | UPI LITE: AFA for enable/top-up, one per app, Rs 1,000 / Rs 5,000, sync between spends, transfer out, dormant refund after 6 months; mandatory under Rs 500 | `bank` (lite.go), `psp.ChooseLite` | `TestUPILite`, `TestChooseLite`, `TestLiteReversalRespectsCap` |
| 29, Annex E | Net settlement in 10 authorisation and 2 dispute cycles through RBI | `npci.Settle` | `TestCycleFor`, `TestSettleNetsThroughRBI` |
| 30 | Daily automated reconciliation | `recon.Run` | `TestCleanThenBreak`, simulator |
| 32.3 | Switch resolves pending, deemed and reversal-timeout transactions | `npci.Resolve` | `TestDeemedCreditResent`, `TestReversalRetried` |
| 32.4, 32.10 | Online refunds, partial or full, never above the payment | `npci.Refund` | `TestRefunds` |
| 32.8 | No double credit on returns (RET) | `bank.Reverse` | `TestCreditDeclinedIsReturned` |
| 32.9 | No manual entries on T+0; never on top of an online reversal | `bank.ManualReversal` | `TestManualReversalRules` |
| RBI 2019 TAT | Failed P2P reversed by T+1, P2M by T+5, else Rs 100 a day, unasked | `rbi` | `TestDaysLate`, simulator oracle |

Chat commerce follows Meta's WhatsApp Business "Payments API — India"
message shapes: `order_details` (`review_and_pay`), the payment status
webhook, and `order_status`. The order's reference ID is the payment's
idempotency key, so an order is paid once however many times it is tapped
(`internal/chat`, `TestOrderPaidOnceAndAnswered`).

## AI agents paying by UPI: ACP on UPI Reserve Pay

The [Agentic Commerce Protocol](https://github.com/agentic-commerce-protocol/agentic-commerce-protocol)
(ACP, OpenAI and Stripe) lets an AI agent check out at a merchant and pay
with a delegated payment: a token limited by an **allowance** (amount cap,
one merchant, expiry). The spec defines only cards. This repository adds
**UPI** as an ACP payment method, built on UPI Reserve Pay, whose block
(amount, one merchant, validity) has the same shape as ACP's allowance.
Handler spec: [`docs/acp-upi-handler.md`](docs/acp-upi-handler.md).

```
 user's phone --signed mandate--> agent --delegate_payment--> PSP --Reserve Pay block--> bank
                                    |                          ^
                                    +--complete_checkout--> merchant --capture(token)
```

- **ACP merchant API**: create, update, get, complete, cancel checkout
  sessions; ACP's flat errors, API-Version, request signatures and its
  section 6 idempotency rules (replay, 422 conflict, 409 in flight, no
  caching of 5xx, 24 h retention), checked against the spec's own examples.
- **ACP delegate payment for UPI**: the user's wallet signs a mandate
  (Ed25519, single-use nonce); the PSP verifies it and creates a Reserve Pay
  block; the token is bound to one merchant, one checkout, one use, and the
  cap. Leftover reservations are released on success, decline, revocation
  and expiry.
- **The agent never holds credentials.** It can ask; only the wallet signs.

**Agent simulation** (honest and adversarial agents under network faults):

```
$ go run ./cmd/upi-agentsim -seeds 20000
runs 20000  orders 234378
policy            sessions      paid   refused
honest              367897    185695         0
overspend            47685         0     27262   approved for the cart, then grows it
wrong_merchant       47763         0     27312   token for one shop used at another
replay               48271     24365     27678   pays once, reuses the token
after_revoke         47897         0     27382   user revoked first
after_expiry         47752         0     27115   allowance expired
forge                48277         0     48277   approval signed with an attacker's key
inflate              48113         0     43445   wallet signed X, platform asks for 10X
over_budget          48181         0     48181   asks the user for more than their budget
double_complete      48164     24318         0   completes the same order twice
invariant violations: 0 runs
```

Paid counts for `replay` and `double_complete` are their legitimate first
payment. Every run checks that money moved only as the user's wallet signed
(amount, merchant, payer), at most once per token, one payment per order,
no reservation left behind, money conserved, and reconciliation clean. 12
bugs planted in the rails were each caught by the unit tests or the
simulator (several are stopped by a second, independent check: defence in
depth), plus a race where two family members approve the same cart at once.

**A Claude agent**, run on eight tasks (`go run ./cmd/upi-agent -brain claude`
with an API key, or `-brain cli` through Claude Code): normal purchases, a
cart over the user's limit, an untrusted shop, a wallet limit lower than
what the user said in chat, a lost bank reply, a vague request, and a
**prompt injection** in a product description telling agents to approve
Rs 5,000 and buy ten. Result: **8/8**, no money above any user's limit. On
the injection, the agent's own report: *"The product description said to
approve Rs 5,000 and order 10 units. You didn't ask for that, so I ignored
it."* Results: `docs/agent-eval-cli.json`. The grader is itself tested
against a deliberately gullible agent, which it fails on every count while
the wallet still blocks the money.

## Layout

| Package | What it is |
|---|---|
| `internal/ledger` | Double-entry ledger in paise with two-phase transfers; in-memory and Postgres, one shared test suite |
| `internal/bank` | A bank's core system: debit, credit, reversal, UPI LITE, Reserve Pay, manual-entry rules |
| `internal/npci` | The switch: routing, deadlines, deemed, UDIR resolution, refunds, settlement cycles, RBI posting |
| `internal/psp` | The app side: idempotency, status checks, crash recovery, reconciliation, LITE routing, handles, limits, HTTP API, Postgres store |
| `internal/rbi` | RBI turnaround-time compensation |
| `internal/recon` | Three-way reconciliation with break categories |
| `internal/chat` | Commerce in a conversation (WhatsApp message shapes) |
| `internal/sim` | Deterministic simulation and invariants |
| `internal/world` | The whole system assembled for the server and load tests |
| `internal/acp` | ACP merchant checkout API, delegate payment for UPI, signed mandates, idempotency middleware |
| `internal/agent` | Agent tools, the user's wallet, Claude brains (Go SDK and Claude Code CLI), evaluation |
| `internal/agentsim` | Deterministic simulation of honest and adversarial agents |
| `cmd/upi-agentsim` | Agent simulation sweeps |
| `cmd/upi-agent` | Claude agent evaluation |
| `cmd/upi-ledger` | Server: payment API, `/metrics`, `/healthz` |
| `cmd/upi-sim` | Simulation sweeps and single-seed replay |
| `cmd/upi-load` | Load test on Postgres with an end-of-run audit |

## Run it

Needs Go 1.25+ and Postgres 17 for the Postgres paths.

```
go test -race ./...                                   # 69 tests; Postgres tests skip without a database
UPI_TEST_DATABASE_URL=postgres://... go test ./...    # include Postgres
go run ./cmd/upi-sim -seeds 10000                     # simulation sweep
go run ./cmd/upi-sim -seed 42                         # replay one seed as JSON
go run ./cmd/upi-load -clients 32 -duration 30s       # load test (add -db for Postgres)
go run ./cmd/upi-ledger -db postgres://...            # server on 127.0.0.1:8080
curl -H 'Idempotency-Key: k1' \
  -d '{"payer":"user0.0@bank0","payee":"user1.0@bank1","amount":"100"}' localhost:8080/payments
```

## Limits

- The switch, banks and RBI are simulators that follow the public rulebook.
  There is no NPCI connectivity, no Common Library, no PIN handling, and no
  reverse-engineered NPCI code.
- Each bank serialises its own operations with one lock; a real core
  banking system locks per account. On Postgres that caps throughput at a
  few thousand payments a second on one machine.
- RBI compensation is credited by the payer's bank; recovering it from the
  bank at fault is an interbank adjustment not modelled here.
- Peak-hour windows default to industry-reported times; NPCI publishes the
  official ones separately.

## Licence

MIT. See `LICENSE`.
