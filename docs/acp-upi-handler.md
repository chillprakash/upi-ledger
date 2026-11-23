# UPI Reserve Pay Payment Handler for ACP

* **Handler name:** `io.github.chillprakash.upi.reserve_pay`
* **Handler id (in checkout responses):** `upi_reserve_pay`
* **Version:** `2026-11-23`
* **ACP version:** `2026-04-17`
* **Status:** proposal with a reference implementation in this repository (`internal/acp`)

## Introduction

The Agentic Commerce Protocol's delegate-payment API defines one payment
method, `card`. This handler lets an AI agent pay a merchant by **UPI**
instead, using **UPI Reserve Pay** (NPCI UPI Consolidated Circular,
18 Sep 2026, section 16): the user blocks funds in their own bank account
for one merchant, and the merchant debits against that block.

ACP's `allowance` already has the shape of a Reserve Pay block:

| ACP allowance | UPI Reserve Pay |
|---|---|
| `max_amount` | block amount |
| `merchant_id` | the one merchant a block is for |
| `expires_at` | block validity |
| `reason: one_time` | single debit, rest released |
| vault token | block reference |

### Key benefits

* The agent never sees bank credentials or a UPI PIN; it holds a one-time token.
* The user approves on their own device with a signed mandate; the PSP
  creates the block only for a mandate whose signature verifies.
* Limits are enforced three times: by the PSP at capture, by the mandate,
  and by the bank's block itself.

## Participants

| Participant | Role |
|---|---|
| **User's wallet** | The user's UPI app on their phone. Holds a signing key registered with the PSP after device binding; signs mandates within the user's own rules. |
| **Agent platform** | Runs the AI agent; calls the merchant's checkout API and the PSP's delegate-payment API. |
| **PSP** | The user's UPI app provider. Verifies mandates, creates Reserve Pay blocks at the user's bank, issues vault tokens, captures. |
| **Merchant** | Exposes ACP checkout; advertises this handler; captures with the token. |
| **Banks, NPCI** | Hold the money; route the debit; settle. |

```
 wallet --signed mandate--> agent platform --delegate_payment--> PSP --block--> user's bank
                                 |                                ^
                                 +--complete_checkout--> merchant +--capture(token)
```

## Merchant: handler advertisement

Returned in `capabilities.payment.handlers` of every checkout session:

```json
{
  "id": "upi_reserve_pay",
  "name": "io.github.chillprakash.upi.reserve_pay",
  "display_name": "UPI (Reserve Pay)",
  "version": "2026-11-23",
  "spec": "https://github.com/chillprakash/upi-ledger/blob/main/docs/acp-upi-handler.md",
  "requires_delegate_payment": true,
  "requires_pci_compliance": false,
  "psp": "upi-ledger",
  "config": {"merchant_id": "ravistores", "merchant_vpa": "shop@icici", "currency": "inr", "max_allowance_hours": 48}
}
```

## Agent platform: delegate payment

`POST /agentic_commerce/delegate_payment` with ACP's headers
(`Authorization`, `API-Version`, `Idempotency-Key`; `Signature` and
`Timestamp` recommended).

```json
{
  "payment_method": {"type": "upi_reserve_pay", "vpa": "asha@hdfc"},
  "allowance": {
    "reason": "one_time", "max_amount": 26250, "currency": "inr",
    "checkout_session_id": "cs_ravistores_000001", "merchant_id": "ravistores",
    "expires_at": "2026-11-23T07:30:00Z"
  },
  "metadata": {"source": "agent"},
  "user_mandate": {
    "mandate": {
      "payer_vpa": "asha@hdfc", "merchant_id": "ravistores", "max_amount": 26250,
      "currency": "inr", "checkout_session_id": "cs_ravistores_000001",
      "expires_at": "2026-11-23T07:30:00Z", "nonce": "asha-phone-17"
    },
    "key_id": "asha-phone",
    "signature": "<base64url Ed25519 over the canonical JSON of mandate>"
  }
}
```

The PSP **MUST**:

1. Require `payment_method.type = upi_reserve_pay`, `reason = one_time`,
   `currency = inr`, a positive `max_amount` in paise, a known
   `merchant_id`, and `expires_at` in the future and within 48 hours.
2. Require a `user_mandate` whose fields equal the request
   (`422 mandate_mismatch` otherwise), whose key is registered to
   `payer_vpa`, whose signature verifies, which has not expired, and whose
   nonce is unused (`401 mandate_invalid` otherwise).
3. Create a Reserve Pay block for `max_amount` at the payer's bank, for the
   merchant, until `expires_at` (`422 insufficient_funds` if the account
   cannot cover it). A new mandate for the same payer and merchant releases
   an unused earlier block (one active block per merchant, 16.4).
4. Return `201` with `{"id": "vt_upi_...", "created": ..., "metadata": {...}}`.

## Merchant: complete and capture

`POST /checkout_sessions/{id}/complete`:

```json
{"payment_data": {"handler_id": "upi_reserve_pay",
  "instrument": {"type": "upi_reserve_pay", "credential": {"type": "vault_token", "token": "vt_upi_..."}}}}
```

The merchant moves the session to `in_progress` before capturing, so a
second complete (any idempotency key, any token) cannot capture again. The
PSP captures only if the token exists, is not revoked or expired, belongs to
this merchant and this checkout session, the amount is positive and at most
`max_amount`, and the token has not been used. After a final outcome, what
is left of the block is released at once.

| Capture outcome | Session | Order |
|---|---|---|
| UPI success | `completed` | created, `order_create` event |
| UPI failure, or any rule refused | `ready_for_payment`, error message `payment_declined` | none |
| UPI outcome not final (deemed, lost reply) | `in_progress` | created when it settles |

## Security considerations

* **Overspend:** the cap is checked at capture and again by the bank block.
* **Wrong merchant / replay:** tokens are bound to one merchant, one
  session and one use.
* **Forged approval:** mandates verify against the key registered to the
  payer; nonces are single use.
* **Prompt injection:** money limits do not depend on the agent following
  instructions; the agent may ask, only the wallet signs.
* **Stale reservations:** blocks are released on success, decline,
  revocation and expiry.

Evidence: `internal/acp` tests (each rule), `internal/agentsim` (thousands
of honest and adversarial agent sessions under network faults, zero
violations), and `cmd/upi-agent` (a Claude agent on eight tasks including a
prompt injection).
