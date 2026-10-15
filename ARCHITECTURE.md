# Architecture

```
 Freelancer UI / API                           PayPal sandbox
 ┌──────────────┐   POST /invoices      ┌──────────────────────────┐
 │  Dashboard   │ ────────────────────► │ Invoicing API (create+send)
 │  (embedded)  │                       └──────────────────────────┘
 │              │   POST .../advance            ▲        │ client pays link
 │              │ ────────┐                     │        ▼
 └──────────────┘         ▼                     │  ┌───────────────┐
                 ┌─────────────────┐  Payouts   │  │ INVOICE.PAID  │
                 │  Rule engine    │ ─────────► │  │   webhook     │
                 │  + Claude review│            │  └──────┬────────┘
                 │  (tighten only) │            │         │ verify signature
                 └────────┬────────┘            │         ▼
                          ▼                     │  settle(): repay advance,
                 ┌─────────────────┐            │  pay remainder (Payouts)
                 │ Double-entry    │ ◄──────────┴──────────┘
                 │ ledger (cents)  │
                 └─────────────────┘
```

## Invoice lifecycle

`sent` → `advancing` → `advanced` → `settling` → `settled`
with side exits: `declined` (rules or AI said no), `cancelled` (cancelled before advance),
`defaulted` (cancelled or overdue after advance; written off).

`advancing` and `settling` are transient locks that block concurrent requests on the same
invoice. Payout calls use deterministic request IDs (`adv-<id>`, `rem-<id>`), so if a
process dies mid-flight and the step is retried, PayPal returns the original result instead
of paying twice.

## Underwriting

1. **Rules** (`internal/underwriting`): risk from paid/late/defaulted counts and average
   days late; thin history adds risk. Advance rate falls and fee rises with risk. Hard
   declines for clients with no history, two or more defaults, risk above 0.6, a freelancer
   invoicing themselves, and invalid or oversized invoices.
2. **Claude review** (`internal/ai`): sees the invoice description (treated as untrusted),
   parties, history and the rule output. Returns a verdict, a multiplier in [0,1], a plain
   explanation and red flags. `Apply` only ever lowers the advance.
3. **Exposure cap**: total outstanding advances are reserved under a mutex before any money
   moves, and released if the payout fails.

## Ledger

Accounts: `cash`, `advances_receivable`, `fee_revenue`, `freelancer_payable`,
`credit_losses`. Sign convention: debit positive, credit negative; every entry sums to zero
and is keyed for idempotency.

| Event | Entry |
| --- | --- |
| Advance paid | receivable +(advance+fee), cash -advance, fee_revenue -fee |
| Client pays | cash +paid, receivable -owed, freelancer_payable -(paid-owed) |
| Remainder paid | freelancer_payable +rem, cash -rem |
| Default | credit_losses +outstanding, receivable -outstanding |

`GET /ledger` returns `balanced: true` when the books sum to zero and shows current exposure.

## Webhooks

`POST /webhooks/paypal` verifies the signature through PayPal's
`/v1/notifications/verify-webhook-signature`, de-duplicates on the event ID, then handles
`INVOICING.INVOICE.PAID` (settle) and `INVOICING.INVOICE.CANCELLED` (cancel or write off).
Failures return 5xx so PayPal retries; every step is idempotent so retries are safe.

## Persistence

State (invoices, ledger entries, client histories, seen event IDs) is snapshotted to
`DATA_FILE` as JSON after each change and reloaded at start. A background sweeper writes
off advances unpaid after `DEFAULT_AFTER_DAYS`.

## Offline mock

`cmd/mockpaypal` implements just enough of the PayPal REST API (OAuth, Invoicing, Payouts,
webhook verification) and fires a webhook when a mock invoice is "paid", so the whole flow,
including the webhook path, runs with no credentials. The test suite uses the same mock.

## Why these trade-offs

- **Standard library only**: nothing to audit or update, a ~6 MB static binary, trivial deploys.
- **LLM as a constrained reviewer, not a decider**: judges can see the AI adds real signal
  (explanations, red flags) while the money path stays deterministic and testable.
- **In-memory + snapshot**: honest for a demo; the `ledger` and state packages are the seam
  where Postgres would slot in.
