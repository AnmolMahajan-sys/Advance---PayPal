# Devpost submission text (copy and paste)

Fill the `<…>` placeholders after you deploy and record the video.

## Project name
Advance

## Tagline
Get paid today for the invoice your client pays next month.

## Elevator pitch (under 200 characters)
Instant invoice financing for freelancers on PayPal: Claude reviews the risk, PayPal Payouts sends the cash in seconds, and a webhook settles the advance when the client pays.

## Inspiration
Freelancers and small sellers routinely wait 30 to 60 days to be paid, while rent and tools are due now. Invoice financing exists, but it is slow, opaque and built for big companies. PayPal already holds the invoice, the client and the payment rails, so the pieces for an instant, explainable advance are all there.

## What it does
- A freelancer sends an invoice through the PayPal Invoicing API and can ask for an advance on it.
- A rule engine prices the risk from the client's payment history and sets the advance rate and fee.
- Claude reviews each case, reads the invoice description and context, and returns a plain-English explanation and red flags. It can only reduce or decline an offer, never raise it.
- Approved advances are paid immediately with PayPal Payouts.
- When the client pays, PayPal's invoice-paid webhook settles the advance in a double-entry ledger and pays the freelancer the remainder. Cancelled or long-overdue invoices are written off and lower that client's score.
- A live dashboard shows invoices, decisions with the AI's reasoning, exposure, and a ledger that must always balance.

## How we built it
- **Go**, standard library only: one static binary, an embedded dashboard, no third-party dependencies.
- **PayPal developer platform (sandbox)**: OAuth2, Invoicing v2 (create and send), Payouts (idempotent via `PayPal-Request-Id`), and Webhooks with PayPal's signature-verification API.
- **Claude (Anthropic API)** as a constrained underwriting reviewer. Its output is clamped to a verdict and a 0-1 multiplier, so a malicious invoice description (prompt injection) cannot increase a payout. If the API is down, Advance falls back to the rules and says so.
- **Double-entry ledger** in integer cents with idempotency keys; exposure caps reserved under a lock.
- **Render** for hosting (Docker blueprint included), **Postman** collection and an **OpenAPI** spec for the API.
- An offline PayPal mock lets anyone run the whole flow, webhook included, with no credentials, and powers the end-to-end tests.

## Challenges
- Making money movement safe to retry: webhook redeliveries, crashed requests and double clicks must never pay twice.
- Letting an LLM add real value to a financial decision without letting it control the money.
- The sandbox has no real credit data, so we modelled client histories explicitly and say so openly.

## Accomplishments
- The full lifecycle works end to end: invoice, underwriting, instant payout, webhook settlement, default write-off.
- The AI can never increase risk, and that property is covered by tests.
- Everything runs offline with one command: `make demo`.

## What we learned
Treating the model as a reviewer with a narrow, validated output contract makes AI useful in a money flow. The deterministic layer stays testable and the model adds explanation and anomaly-spotting.

## What's next
Postgres and Redis for durable state and exposure limits, real client-history signals, factoring pools funded by investors, and support for Venmo and Braintree payers.

## Built with
Go, PayPal (Invoicing, Payouts, Webhooks, sandbox), Claude API, Render, Postman, OpenAPI, Docker, GitHub Actions

## Links
- GitHub: <https://github.com/YOU/advance>
- Live demo: <https://advance.onrender.com>
- Video: <https://youtu.be/…>

## Testing instructions for judges
Easiest, no credentials: clone the repo, install Go 1.22+, run `make demo`, open http://localhost:8080.
Create an invoice for `reliable@example.com`, click **Get advance**, then **Simulate client paying**.
Create one for `risky@example.com` to see a decline. The hosted demo (if provided) has the same buttons.
