# NEXT_STEPS: everything to do after downloading the zip

Work through this top to bottom. Phases 1-2 take about 15 minutes and need no accounts.
Budget the rest over a few days. **Deadline: November 12, 2026, 12:00 PM PT, which is
November 13, 1:30 AM IST.** Aim to submit by **November 10** so a Devpost hiccup cannot cost you.

Honest note: I could not reach PayPal from my environment, so everything is tested against a
faithful local mock, not the live sandbox. Phase 4 is where you confirm the real thing; budget
time to fix small request-shape differences there (see "If something fails").

---

## Phase 1: Unpack and run it offline (10 min)

1. Install **Go 1.22 or newer** (https://go.dev/dl/). Check with `go version`.
2. Unzip, then in a terminal:
   ```sh
   cd advance
   make test        # all tests should pass
   make demo        # starts Advance + a PayPal mock
   ```
   Windows without `make`/bash: run `go test ./...`, then in two terminals
   `go run ./cmd/mockpaypal` (set `ADDR=:9090` and `WEBHOOK_TARGET=http://localhost:8080/webhooks/paypal`)
   and `go run ./cmd/advance` (set `ADDR=:8080 PAYPAL_BASE_URL=http://localhost:9090 PAYPAL_CLIENT_ID=demo PAYPAL_CLIENT_SECRET=demo DEMO_MODE=true`).
   Easiest on Windows: use WSL.
3. Open http://localhost:8080. Create an invoice for `reliable@example.com`, click
   **Get advance**, then **Simulate client paying**. Confirm the ledger shows `balanced`.
   Create one for `risky@example.com` and confirm it is declined.

## Phase 2: Make it yours (10 min)

1. Open `LICENSE` and put your legal name in the copyright line (currently "Anmol").
2. Search the repo for `YOU` and `<` placeholders (`SUBMISSION.md`) and note what to fill later.
3. Create the GitHub repo now so history shows work across the submission window:
   ```sh
   git init && git add . && git commit -m "Advance: AI-underwritten invoice financing on PayPal"
   git branch -M main
   git remote add origin https://github.com/<you>/advance.git
   git push -u origin main
   ```
   Make the repo **public**. On GitHub the license appears in the **About** sidebar automatically
   when `LICENSE` is detected as MIT; open the repo and confirm it says "MIT license" there
   (Devpost requires it to be visible). Add topics: `paypal`, `golang`, `fintech`, `claude`, `hackathon`.
4. Confirm CI is green (Actions tab). Fix anything red before moving on.

## Phase 3: Get the credentials (30 min)

### PayPal sandbox
1. Go to https://developer.paypal.com and log in (a normal PayPal account works).
2. **Apps & Credentials**, switch the toggle to **Sandbox**, **Create App** (type: Merchant).
   Copy the **Client ID** and **Secret**.
3. In the app page, check the **Features** section and make sure **Payouts** (and Invoicing, if listed) are enabled.
4. **Testing Tools, Sandbox Accounts**: you need three accounts.
   - The **Business** account tied to your app: it sends invoices and funds payouts. Check its balance
     is funded (sandbox accounts normally start with money; add funds in the sandbox dashboard if not).
   - A **Personal** account for the **freelancer** (receives advances). Note its email.
   - A **Personal** account for the **client/buyer** (pays invoices). Note its email.
5. Keep sandbox account passwords handy; you will log in as the buyer to pay an invoice.

### Anthropic (for the AI review)
1. Create an API key at https://console.anthropic.com and add a few dollars of credit.
2. Check the current model name in the Anthropic docs and set `ANTHROPIC_MODEL` if you want a different one
   (default is `claude-sonnet-5-5`). Without a key the app still works, rules-only.

## Phase 4: Run against the real PayPal sandbox (1-2 hours)

1. Make a public URL for webhooks. Pick one:
   - **Fast (local):** install ngrok and run `ngrok http 8080`; note the `https://….ngrok-free.app` URL.
   - **Or skip ahead to Phase 5** and deploy on Render first, then use that URL.
2. In the PayPal app page, **Add Webhook**:
   - URL: `https://<public-url>/webhooks/paypal`
   - Events: **Invoicing: Invoice paid** (`INVOICING.INVOICE.PAID`) and **Invoice cancelled** (`INVOICING.INVOICE.CANCELLED`)
   - Save, then copy the **Webhook ID**.
3. Configure:
   ```sh
   cp .env.example .env
   ```
   Fill in `PAYPAL_CLIENT_ID`, `PAYPAL_CLIENT_SECRET`, `PAYPAL_WEBHOOK_ID`, `ANTHROPIC_API_KEY`, and give your
   sandbox buyer a payment history, otherwise Advance (correctly) refuses to finance an unknown client:
   ```
   DEMO_CLIENTS=<buyer-sandbox-email>=reliable
   ```
   Keep `DEMO_MODE=true` for the demo buttons. Do **not** set `PAYPAL_BASE_URL` (it defaults to the sandbox).
4. `make run`, open http://localhost:8080 (or your public URL).
5. Run the real flow:
   1. Create an invoice: freelancer = your freelancer sandbox email, client = your buyer sandbox email.
      Check the invoice appears in the **business** sandbox account (sandbox.paypal.com, log in as the business, Invoicing).
   2. Click **Get advance**. Check the freelancer personal account received the payout and the dashboard shows Claude's explanation.
   3. Pay the invoice as the buyer: open the invoice's pay link from the dashboard (or log in to
      sandbox.paypal.com as the buyer and pay it from Activity / the invoice link).
   4. Within seconds the webhook should fire: invoice becomes **settled**, the ledger stays `balanced`,
      and the freelancer gets the remainder. Check the server logs.
6. Also exercise: a risky client (`DEMO_CLIENTS=<email>=risky`) being declined, and cancelling an advanced
   invoice in PayPal to see it written off as a credit loss.

### If something fails
- **401/invalid_client:** wrong credentials, or you used Live credentials. Use the Sandbox toggle.
- **Payout fails (permission/insufficient funds):** enable Payouts on the app and fund the business account.
- **Invoice create/send returns a 4xx:** the log prints PayPal's full error body. Open
  `internal/paypal/client.go` (`CreateDraftInvoice`, `SendInvoice`, `CreatePayout`) and adjust the request
  to match the error; PayPal's Invoicing v2 docs list required fields. Re-run `make test` after changes.
- **Webhook returns 401 "Invalid webhook signature":** `PAYPAL_WEBHOOK_ID` must be the ID of the webhook
  pointing at your current URL (ngrok URLs change every run; update the webhook when they do).
- **Webhook never arrives:** check the PayPal dashboard's webhook event log and that the URL is reachable.
- Anything stuck: the **Simulate client paying** button settles an advanced invoice without PayPal, so you can still demo.

## Phase 5: Deploy a hosted demo on Render (30 min)

1. Push to GitHub (Phase 2), then on https://render.com: **New, Blueprint**, pick the repo. It reads `render.yaml`.
2. Fill the secrets when prompted (`PAYPAL_CLIENT_ID`, `PAYPAL_CLIENT_SECRET`, `PAYPAL_WEBHOOK_ID`,
   `ANTHROPIC_API_KEY`) and add `DEMO_CLIENTS` as in Phase 4.
3. When it is live, open the URL and run the flow. Update your PayPal sandbox webhook URL to
   `https://<your-service>.onrender.com/webhooks/paypal`.
4. The free plan sleeps after inactivity (first request is slow) and has no disk, so data resets on restart.
   Open the site a minute before recording or judging. This is also your **Render sponsor prize** entry.
5. Set `API_KEY` only if you want the JSON API locked (the dashboard then prompts for the key); for a
   public judge demo leave it unset.

## Phase 6: Optional sponsor-prize extras (each is cheap)

- **Postman:** import `docs/postman_collection.json`, set `baseUrl` to your Render URL, run it once, and
  optionally publish the collection publicly; link it in the submission.
- **APIMatic:** import `docs/openapi.yaml` and generate an SDK (Go or Python); commit it under `sdk/` and mention it.
- Only claim a sponsor tool in the submission if you actually used it.

## Phase 7: Record the demo video (under 3 minutes, public on YouTube)

Use the hosted URL or localhost, screen record at 1080p (OBS or QuickTime), narrate live or voice over.

| Time | Show | Say |
| --- | --- | --- |
| 0:00-0:20 | Dashboard hero | The problem: freelancers wait 30-60 days; Advance pays today, on PayPal. |
| 0:20-0:50 | Create invoice (real sandbox) | Sent through the PayPal Invoicing API; show it in the sandbox business account. |
| 0:50-1:30 | Get advance | Rules price it, Claude explains and flags risk; payout lands via PayPal Payouts; show the freelancer account. |
| 1:30-2:00 | Risky client | Declined; the AI can only tighten, never raise, so prompt injection cannot inflate a payout. |
| 2:00-2:30 | Client pays, webhook | Signature-verified webhook settles; ledger balanced; remainder paid. |
| 2:30-2:50 | Repo + architecture | Go stdlib only, idempotent money paths, tests, Render, MIT. |

Tips: do a dry run first; keep the cursor slow; do not show secrets (`.env`, API keys); upload as
**Public** (not unlisted is safest) and confirm it plays while logged out; the video must be under 3:00.

## Phase 8: Submit on Devpost

1. Open https://paypalaihackathon.devpost.com and register/join if you have not.
2. **Create project** and paste from `SUBMISSION.md`, filling the GitHub, live demo and video links.
3. Upload 3-4 images: use `docs/screenshots/*.png`.
4. Add the **Built with** tags, select sponsor tools you truly used, and submit before **Nov 12, 12:00 PM PT**.
5. Re-check the checklist below, then submit. You can edit until the deadline.

### Final checklist
- [ ] Repo is public and the About box shows **MIT license**
- [ ] README run instructions work from a fresh clone (try it in a new folder)
- [ ] No secrets committed (`git log -p | grep -i secret` should find nothing real; `.env` is git-ignored)
- [ ] Hosted demo loads, or setup instructions are clear (`make demo` needs no credentials)
- [ ] Video is public, under 3:00, shows the working end-to-end flow, in English
- [ ] Devpost text matches what the app really does (do not overclaim; the limits are in the README)
- [ ] Project was built/updated inside the Oct 1 to Nov 12 window (your commit history shows it)

## Suggested schedule from today (Oct 15)

| Dates | Goal |
| --- | --- |
| Oct 15-17 | Phases 1-3: run offline, push to GitHub, get credentials |
| Oct 18-24 | Phase 4: real sandbox working, fix request shapes |
| Oct 25-28 | Phase 5-6: Render deploy, Postman/APIMatic extras |
| Oct 29-Nov 3 | Polish and stretch goals (below); screenshots from the real sandbox |
| Nov 4-7 | Phase 7: record and edit the video |
| Nov 8-10 | Phase 8: submit, then re-verify everything works |

## Stretch goals if you have time (most valuable first)

1. Swap the JSON snapshot for **Postgres** (you know it well); the state and ledger packages are the seam. Mention it in the pitch.
2. Add **Redis** exposure limits with a Lua script, reusing your DistriLimit approach.
3. Let the AI also scan the invoice's line items for anomalies (inflated rates, duplicate invoices) and show them in red flags.
4. A second payer path (Venmo/Braintree sandbox) to strengthen "Best Use of PayPal".

## Honest caveats to keep in your pitch
Client histories are synthetic (the sandbox has no credit data); state is in-memory plus a snapshot;
this is a sandbox prototype and not a licensed lending product. Saying so plainly reads as maturity to judges.
