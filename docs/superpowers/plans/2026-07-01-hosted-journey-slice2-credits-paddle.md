# Hosted journey slice 2: credits in Overview + Paddle prepaid top-up + money-safety

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make available credit first-class in the Overview, let a user set spend caps, and let a user buy more credit via a Paddle hosted checkout (prepaid top-up), with the webhook crediting the ledger idempotently and the money-safety stores made durable.

**Architecture:** Extends the existing provider-neutral billing seam. Paddle is already selected when its keys are present (`cmd/console/billing.go`); this slice adds a `CheckoutURL` (verified shape: `POST /transactions` with an inline custom-amount price on one "credit top-up" product, read `checkout.url`), a console endpoint to start a top-up, a spend-cap endpoint, the SPA UI for both plus the Overview credit element, a webhook path that records a `KindTopUp` ledger entry, and Postgres backing for the customer map and spend-cap store. Card capture and management stay in Paddle's hosted checkout/portal (Paddle is Merchant of Record); we link, we do not build a card form.

**Tech Stack:** Go (`internal/saas/billing`, `internal/saas/billingprovider/paddle`, `internal/saas/billingprovider`, `internal/saas/console`, `internal/saas/pgstore`, `cmd/console`), React 18 + TanStack Query (`web/app`), Vitest, `@mitos/brand`.

**Verified Paddle shape (from a live sandbox call, 2026-07-01):**
- `POST {base}/transactions` with `Authorization: Bearer <key>` and body:
  `{"items":[{"quantity":1,"price":{"product_id":"<topup-product>","description":"...","unit_price":{"amount":"<cents>","currency_code":"EUR"},"tax_mode":"account_setting","quantity":{"minimum":1,"maximum":1}}}],"collection_mode":"automatic","custom_data":{"kind":"credit_topup","org_id":"<org>","amount_cents":"<cents>"}}`
- Response `data.id` is the transaction id; `data.checkout.url` is the hosted checkout URL (uses the account's default payment link domain). `custom_data` round-trips back on the webhook.
- Base URLs: live `https://api.paddle.com`, sandbox `https://sandbox-api.paddle.com`.

## Global Constraints

- No em (U+2014) or en (U+2013) dashes anywhere: source, comments, tests, commit messages. ASCII hyphen only.
- Money is integer cents (`billing.Money`); never float for accounting. Amounts are sent to Paddle as a string of cents.
- Provider API keys, webhook secrets, and any payment detail are NEVER logged, NEVER in errors, NEVER in condition messages. Ids and counts only.
- Go: `fmt.Errorf("ctx: %w", err)`; gofmt + `go vet` clean; both lints (`golangci-lint run --timeout=5m` AND `GOOS=linux golangci-lint run --timeout=5m`).
- Fluorescence tokens only in SPA chrome; `@mitos/brand` components; approved numbers only; brand voice; no dashes.
- Provider account, key, webhook secret, top-up product id, and currency are DEPLOY-TIME CONFIG (env), never hardcoded. The account in use for verification is a Paperclip test account; production will be a mitos account. Do not hardcode any account-specific id in non-test code.
- TDD: failing test first, same commit. DCO: `git commit -s`. Conventional prefixes. Stage explicit paths only.
- Work in the worktree `/Users/jannesstubbemann/repos/mitos-run/mitos-journey` (branch `feat/hosted-journey-finish`). Go from the worktree root; SPA from `web/app`. Test Postgres DSN (for pgstore tasks): `postgres://postgres:test@127.0.0.1:55432/mitos_fresh?sslmode=disable` (recreate fresh; pgstore tests Open-before-truncate). If Postgres is not running, the pgstore tests are the only ones that need it; note it in the report rather than skipping silently.
- Mirror existing patterns: `internal/saas/console/portal.go` (the billing.manage-gated endpoint), `internal/saas/billingprovider/paddle/paddle.go` + `paddle_test.go` (provider + fake-server test), `internal/saas/billingprovider/billingprovider.go` (the Provider seam + webhook handler + `OrgCustomers`), `internal/saas/billing/ledger.go` (`TopUp`, `TopUpLadder`, `KindTopUp`), `web/app/src/views/Billing.tsx` + `web/app/src/views/Instruments.tsx` + `web/app/src/data/account.ts`.

---

### Task D1: available credit first-class in the Overview

Surface the org's available credit as a persistent, prominent element on the Overview (not only inside the first-run card, and readable even when the deeper billing view is gated). Reuse the existing billing read.

**Files:**
- Modify: `web/app/src/views/Instruments.tsx`
- Test: `web/app/src/views/Instruments.test.tsx`

**Interfaces:**
- Consumes: the existing `useBilling()` hook (returns `{ balance_cents, spend_cents, ... }`) and `fmtDollars` from `../api`.
- Produces: a persistent "Available credit" element in the Overview hero/summary zone showing `fmtDollars(balance_cents)` and, secondarily, spend.

- [ ] **Step 1: Write the failing test**

In `Instruments.test.tsx`, add a test: for an ACTIVE org (so the first-run card is hidden) with `useBilling` returning `{ balance_cents: 500, spend_cents: 123, ... }`, the Overview renders an "Available credit" label and the text `$5.00`. Mock `useBilling` (and `useFirstActivity` as the existing tests do). Assert it renders regardless of whether the deep Billing route is enabled (do not gate the credit readout on `c.billing`; the credit number itself is safe to show).

- [ ] **Step 2: Run to verify it fails**

Run: `cd web/app && pnpm test -- views/Instruments`
Expected: FAIL (no "Available credit" element yet).

- [ ] **Step 3: Implement**

In `Instruments.tsx`, add a first-class "Available credit" element to the Overview summary zone (near the hero/proof band), showing `fmtDollars(billing.balance_cents)` with a small "spent `fmtDollars(spend_cents)`" secondary, using existing brand tokens and the existing StatTile/Card style. Read from `useBilling()`; render a calm placeholder while loading; do not regress the existing panels. Keep the existing credits/spend panels as-is (this is the always-visible headline number).

- [ ] **Step 4: Run to verify it passes, then commit**

Run: `cd web/app && pnpm test && pnpm build`
Expected: PASS, clean.

```bash
git add web/app/src/views/Instruments.tsx web/app/src/views/Instruments.test.tsx
git commit -s -m "feat(console): available credit is first-class on the Overview"
```

---

### Task D2: spend-cap controls (set soft/hard caps from the console)

A `billing.manage`-gated endpoint to set the org spend cap, and the SPA form. This is the guardrail that makes prepaid credit safe.

**Files:**
- Create: `internal/saas/console/spendcap.go` (the `POST /console/billing/spend-cap` handler)
- Modify: `internal/saas/console/console.go` (mount the route) + the console deps (a `SpendCapWriter` seam wrapping the existing `billing` spend-cap setter/store)
- Test: `internal/saas/console/spendcap_test.go`
- Modify: `web/app/src/views/Billing.tsx` (a "Set spend cap" form), `web/app/src/data/account.ts` (a `useSetSpendCap` mutation), `web/app/src/api.ts` (the post)
- Test: `web/app/src/views/Billing.test.tsx`

**Interfaces:**
- Consumes: `saas.PermManageBilling` (the gate; mirror `portal.go`), the existing spend-cap persistence (`billing.SpendCapStore` / a `SetSpendCap` on the billing service).
- Produces: `POST /console/billing/spend-cap` body `{ soft_cents: number, hard_cents: number }` -> 200 (or 403 without billing.manage, 400 on invalid); the SPA mutation + form.

- [ ] **Step 1: Write the failing Go test**

`spendcap_test.go`: a caller WITH billing.manage can POST `{soft_cents, hard_cents}` and the cap is persisted (a subsequent billing view read shows the new caps); a caller WITHOUT billing.manage gets 403; invalid input (negative, or soft > hard when both > 0) is rejected 400. Mirror `portal_test.go` for the permission-gated setup.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/saas/console/ -run SpendCap -v`
Expected: FAIL.

- [ ] **Step 3: Implement the endpoint**

Create `spendcap.go`: decode `{soft_cents, hard_cents}`, validate (non-negative; hard >= soft when both > 0), require `saas.PermManageBilling` (mirror `portal.go`), call the cap writer with `billing.SpendCap{OrgID, SoftCap: Money(soft), HardCap: Money(hard)}`, return 200. Never log amounts beyond org id + counts. Mount `POST /console/billing/spend-cap` in `console.go` next to the portal route. Wire the cap writer in `cmd/console`.

- [ ] **Step 4: Run the Go test, build, vet**

Run: `go test ./internal/saas/console/ -run SpendCap -v && go build ./... && go vet ./internal/saas/console/`
Expected: PASS, clean.

- [ ] **Step 5: Implement the SPA**

In `api.ts` add `setSpendCap(soft, hard)` (POST). In `account.ts` add `useSetSpendCap()` (a mutation invalidating the billing query). In `Billing.tsx` add a "Set spend cap" form (two money inputs, a Save button). On success show a calm aria-live confirmation. Tokens only, `@mitos/brand` inputs/Button, magenta focus ring, no dashes. Update `Billing.test.tsx` to assert the form posts the caps and shows confirmation.

- [ ] **Step 6: Run SPA tests + build, commit**

Run: `cd web/app && pnpm test && pnpm build`
Expected: PASS, clean.

```bash
git add internal/saas/console/spendcap.go internal/saas/console/console.go internal/saas/console/spendcap_test.go cmd/console/ web/app/src/views/Billing.tsx web/app/src/views/Billing.test.tsx web/app/src/data/account.ts web/app/src/api.ts
git commit -s -m "feat(console): set org spend cap from the billing view (billing.manage gated)"
```

---

### Task D3: Paddle CheckoutURL for a prepaid top-up (custom amount)

Add a top-up checkout method to the Paddle provider using the verified `POST /transactions` inline-custom-amount shape, tested against an httptest fake.

**Files:**
- Modify: `internal/saas/billingprovider/paddle/paddle.go` (add `CheckoutURL`)
- Test: `internal/saas/billingprovider/paddle/paddle_test.go`

**Interfaces:**
- Consumes: the Paddle `Config` (api key, base URL) already on the provider; a top-up product id + currency passed in (from config, not hardcoded).
- Produces: `func (p *Provider) CheckoutURL(ctx context.Context, in billingprovider.TopUp) (string, error)` where `type TopUp struct { CustomerRef string; OrgID string; AmountCents int64; ProductID string; Currency string }` (define `TopUp` in `billingprovider` so the console can build it). It POSTs the verified transaction body (inline price on `ProductID`, `unit_price.amount = strconv of AmountCents`, `custom_data{kind:"credit_topup", org_id, amount_cents}`, `collection_mode:"automatic"`, and `customer_id: CustomerRef` when non-empty), then returns `data.checkout.url`.

- [ ] **Step 1: Write the failing test**

In `paddle_test.go`, add a test using an `httptest.Server` standing in for `POST /transactions`: assert the request carries `Authorization: Bearer <key>`, the JSON body has the product id, `unit_price.amount` equal to the cents string, `currency_code`, and `custom_data.org_id`/`amount_cents`; return a JSON `{"data":{"id":"txn_x","checkout":{"url":"https://example/checkout?_ptxn=txn_x"}}}`. Assert `CheckoutURL` returns that URL. Add a case where the response is non-2xx -> wrapped error, and assert the api key never appears in the returned error string.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/saas/billingprovider/paddle/ -run Checkout -v`
Expected: FAIL.

- [ ] **Step 3: Implement CheckoutURL**

Add `CheckoutURL` to the Paddle provider mirroring the `PortalURL` style (bearer auth, short timeout, `fmt.Errorf("ctx: %w", err)`, never log the key or body secrets). Build the exact verified body; parse `data.checkout.url`; if it is empty, return a clear wrapped error ("checkout url missing; set a default payment link in Paddle") without leaking secrets. Define `billingprovider.TopUp` in `billingprovider/billingprovider.go`.

- [ ] **Step 4: Run, build, vet, commit**

Run: `go test ./internal/saas/billingprovider/paddle/ -run Checkout -v && go build ./... && go vet ./internal/saas/billingprovider/...`
Expected: PASS, clean.

```bash
git add internal/saas/billingprovider/paddle/paddle.go internal/saas/billingprovider/paddle/paddle_test.go internal/saas/billingprovider/billingprovider.go
git commit -s -m "feat(billing): Paddle prepaid top-up checkout (custom amount transaction)"
```

---

### Task D4: console top-up checkout endpoint

Expose a `billing.manage`-gated console endpoint that starts a top-up and returns the Paddle checkout URL.

**Files:**
- Create: `internal/saas/console/topup.go` (`GET /console/billing/topup?amount=<cents>` -> `{url}`)
- Modify: `internal/saas/console/console.go` (mount) + `cmd/console/billing.go` (wire the top-up product id + currency from env, and the customer map)
- Test: `internal/saas/console/topup_test.go`

**Interfaces:**
- Consumes: the billing provider's `CheckoutURL` (Task D3); the `OrgCustomers` map; env `MITOS_CONSOLE_PADDLE_TOPUP_PRODUCT` and `MITOS_CONSOLE_PADDLE_CURRENCY` (default `EUR`).
- Produces: `GET /console/billing/topup?amount=<cents>` -> `{ "url": "..." }`, 200; 400 on a non-positive/oversize amount or when top-up is not configured (product id empty); 403 without billing.manage; 404 when the org has no customer mapping (mirror `portal.go`).

- [ ] **Step 1: Write the failing test**

`topup_test.go`: a billing.manage caller GETs `/console/billing/topup?amount=2500` and gets a `{url}` (with a stub provider whose `CheckoutURL` echoes a known url); amount `0`/negative/`abc` -> 400; unconfigured product id -> 400; without billing.manage -> 403; no customer mapping -> 404. Mirror `portal_test.go`.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/saas/console/ -run TopUp -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Create `topup.go`: parse+validate `amount` (positive, a sane ceiling e.g. <= 1_000_000 cents), require `billing.manage`, resolve the org customer ref (404 if missing), build `billingprovider.TopUp{CustomerRef, OrgID, AmountCents, ProductID, Currency}` from the injected config, call `provider.CheckoutURL`, return `{url}`. If the configured product id is empty, 400 with a clear message. Mount the route. Wire the product id + currency in `cmd/console/billing.go` from env (empty disables the affordance).

- [ ] **Step 4: Run, build, vet, commit**

Run: `go test ./internal/saas/console/ -run TopUp -v && go build ./... && go vet ./internal/saas/console/`
Expected: PASS, clean.

```bash
git add internal/saas/console/topup.go internal/saas/console/console.go internal/saas/console/topup_test.go cmd/console/billing.go
git commit -s -m "feat(console): top-up checkout endpoint (billing.manage gated)"
```

---

### Task D5: "Add credits" SPA UI

Let the user pick a top-up amount (the `TopUpLadder` tiers plus a custom amount) and open the Paddle checkout.

**Files:**
- Modify: `web/app/src/views/Billing.tsx` (an "Add credits" area) and optionally surface an "Add credits" link on the Overview credit element (D1)
- Modify: `web/app/src/data/account.ts` + `web/app/src/api.ts` (a `topupUrl(amountCents)` fetch)
- Test: `web/app/src/views/Billing.test.tsx`

**Interfaces:**
- Consumes: `GET /console/billing/topup?amount=<cents>` (Task D4).
- Produces: `api.topupUrl(amountCents): Promise<{url:string}>`; an "Add credits" UI offering preset tiers (mirror the `TopUpLadder` cents values: 1000, 2500, 5000, 10000) plus a custom amount input; clicking a tier fetches the url and `window.open`s it (mirror the existing `onManageBilling` portal pattern).

- [ ] **Step 1: Write the failing test**

In `Billing.test.tsx`, mock `api.topupUrl` to resolve `{url:'https://example/checkout'}` and spy `window.open`; assert the preset buttons render (e.g. `$10`, `$25`, `$50`, `$100`), clicking `$25` calls `topupUrl(2500)` and opens the returned url; the custom-amount input posts its cents value; an error path shows a calm message. Show the affordance only when billing is enabled (capabilities).

- [ ] **Step 2: Run to verify it fails**

Run: `cd web/app && pnpm test -- views/Billing`
Expected: FAIL.

- [ ] **Step 3: Implement**

In `api.ts` add `topupUrl(amountCents)`. In `account.ts` add a helper if needed. In `Billing.tsx` add an "Add credits" section with the preset tiers + a custom-amount field (validated positive), each opening the checkout url via `window.open` (mirror `onManageBilling`). Gate on the billing capability. On error, a calm aria-live message. Tokens only, no dashes.

- [ ] **Step 4: Run SPA tests + build, commit**

Run: `cd web/app && pnpm test && pnpm build`
Expected: PASS, clean.

```bash
git add web/app/src/views/Billing.tsx web/app/src/views/Billing.test.tsx web/app/src/data/account.ts web/app/src/api.ts
git commit -s -m "feat(console): add-credits UI (prepaid top-up tiers + custom amount)"
```

---

### Task D6: webhook records a KindTopUp on a completed top-up

When Paddle reports a completed top-up transaction, credit the org's ledger idempotently by the transaction id.

**Files:**
- Modify: `internal/saas/billingprovider/paddle/paddle.go` (map a completed-transaction event carrying `custom_data.kind == "credit_topup"` into the neutral event) and `internal/saas/billingprovider/billingprovider.go` (the `Event` type + the webhook handler: on a top-up event, record the top-up)
- Modify: the webhook wiring in `cmd/console/billing.go` (give the handler a way to call `billing.TopUp` against the ledger)
- Test: `internal/saas/billingprovider/paddle/paddle_test.go` (event mapping) and `internal/saas/billingprovider/webhook_test.go` (handler records the top-up once, idempotent)

**Interfaces:**
- Consumes: `billing.TopUp(ctx, ledger, orgID, amount, ref, now)` (already exists; `ref` is the transaction id, keyed `topup:<ref>` so replays do not double-credit); the webhook `custom_data.org_id` and the transaction total.
- Produces: an `Event` that carries, for a completed top-up: the org id, the amount (cents), and the transaction id; the webhook handler calls `TopUp` with those. Existing subscription-status events keep their current behavior.

- [ ] **Step 1: Write the failing test**

In `paddle_test.go`, add a test that `VerifyWebhook` on a `transaction.completed` (or `transaction.paid`) event body with `custom_data.kind=credit_topup`, `org_id`, and a grand total maps to an Event whose kind is a top-up with the right org id and cents. In `webhook_test.go`, assert the handler records exactly one `KindTopUp` ledger entry for that event, and that delivering the same event twice does not double-credit (idempotent on the tx id). Reuse the existing signature-fixture pattern in `paddle_test.go`.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/saas/billingprovider/... -run 'TopUp|Webhook' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Extend the Paddle event mapping to recognize a completed transaction with `custom_data.kind == "credit_topup"` and produce a top-up Event (org id from `custom_data.org_id`, cents from the transaction total). Extend the neutral webhook handler so a top-up Event calls `TopUp(ctx, ledger, orgID, Money(cents), txID, now)`. Keep subscription-status events unchanged. Never log the amount beyond org id + cents count; never log the raw body. Wire the ledger into the webhook handler in `cmd/console/billing.go`.

- [ ] **Step 4: Run, build, vet, commit**

Run: `go test ./internal/saas/billingprovider/... -run 'TopUp|Webhook' -v && go build ./... && go vet ./internal/saas/billingprovider/...`
Expected: PASS, clean.

```bash
git add internal/saas/billingprovider/paddle/paddle.go internal/saas/billingprovider/billingprovider.go internal/saas/billingprovider/paddle/paddle_test.go internal/saas/billingprovider/webhook_test.go cmd/console/billing.go
git commit -s -m "feat(billing): credit the ledger on a completed Paddle top-up (idempotent)"
```

---

### Task D7: durable customer map and spend-cap store (money-safety)

Back the customer mapping and the spend-cap store with Postgres so a restart does not lose caps or customer links.

**Files:**
- Create: `internal/saas/pgstore/customers.go` + `internal/saas/pgstore/spendcap.go` (Postgres implementations behind the existing interfaces)
- Create: a migration `internal/saas/pgstore/migrations/0004_billing_customers_and_spendcaps.sql`
- Modify: `cmd/console/billing.go` (select the Postgres implementations when a pool is configured, else the in-memory defaults, matching how other stores auto-select)
- Test: `internal/saas/pgstore/customers_test.go` + `internal/saas/pgstore/spendcap_test.go`

**Interfaces:**
- Consumes: the existing `billingprovider.OrgCustomers` interface and the `billing.SpendCapStore` interface (match their exact method sets from the current code).
- Produces: `pgstore.NewPgCustomers(pool)` and `pgstore.NewPgSpendCapStore(pool)` implementing those interfaces, with round-trip tests.

- [ ] **Step 1: Write the failing tests**

`customers_test.go` and `spendcap_test.go`: gated on the test DSN (mirror the existing pgstore tests' Open-before-truncate setup). Assert a set/get round-trip for the org->customer mapping and for a spend cap (soft/hard), including update and the empty/absent case.

- [ ] **Step 2: Run to verify it fails**

Run: `export MITOS_TEST_DATABASE_DSN="postgres://postgres:test@127.0.0.1:55432/mitos_fresh?sslmode=disable"; go test ./internal/saas/pgstore/ -run 'Customers|SpendCap' -v`
Expected: FAIL (or a clear skip if Postgres is down; if skipped, note it and still implement + build).

- [ ] **Step 3: Implement**

Add the migration (a `billing_customers(org_id PK, customer_ref, updated_at)` table and a `spend_caps(org_id PK, soft_cents, hard_cents, updated_at)` table). Implement `pgstore.NewPgCustomers` and `pgstore.NewPgSpendCapStore` against the existing interfaces, mirroring `pgstore/creditledger.go` style (parameterized SQL, `fmt.Errorf` wrapping, no secrets). In `cmd/console/billing.go`, select the Postgres impls when the pool is configured, else keep the in-memory defaults.

- [ ] **Step 4: Run tests (with test PG), build, vet, commit**

Run: `export MITOS_TEST_DATABASE_DSN="postgres://postgres:test@127.0.0.1:55432/mitos_fresh?sslmode=disable"; go test ./internal/saas/pgstore/ -run 'Customers|SpendCap' -v && go build ./... && go vet ./internal/saas/...`
Expected: PASS (or noted PG-down), clean build.

```bash
git add internal/saas/pgstore/customers.go internal/saas/pgstore/spendcap.go internal/saas/pgstore/migrations/0004_billing_customers_and_spendcaps.sql internal/saas/pgstore/customers_test.go internal/saas/pgstore/spendcap_test.go cmd/console/billing.go
git commit -s -m "feat(billing): durable Postgres customer map and spend-cap store"
```

---

## Self-Review

**1. Spec coverage (slice 2 of the completion spec, Workstream D):** credit first-class in Overview (D1), spend-cap controls (D2), Paddle prepaid top-up checkout end to end (D3 provider method + D4 console endpoint + D5 UI), the webhook crediting the ledger idempotently (D6), and the money-safety durable stores (D7). Box/packaged plans and the openclaw packaged path remain out of scope (spec non-goals). The live Paddle webhook signature round-trip needs the deploy webhook secret and is verified on deploy; the local surface is the fake-server tests plus the existing signature fixtures.

**2. Placeholder scan:** every task names exact files, endpoints, the permission gate (`PermManageBilling`), the verified Paddle body shape, and the ledger call (`TopUp` keyed `topup:<ref>`). The one "confirm against real code" is the exact method set of `OrgCustomers`/`SpendCapStore` (D2/D7), which the implementer reads from the current interfaces; that is a verification instruction, not a placeholder.

**3. Type consistency:** `billingprovider.TopUp` (D3) is built by the console endpoint (D4) and its fields match; `CheckoutURL(ctx, TopUp)` (D3) is the method D4 calls; the top-up Event (D6) carries org id + cents + tx id and the handler calls `billing.TopUp(...)` (existing); `api.topupUrl(amountCents)` (D5) hits `GET /console/billing/topup?amount=` (D4); the durable stores (D7) implement the SAME `OrgCustomers`/`SpendCapStore` interfaces the console/webhook already consume, so selection is a wiring swap in `cmd/console/billing.go`.

## Verification note

After the tasks pass, verify the real outbound path once against the Paperclip sandbox account (a live `CheckoutURL` call returning a real `checkout.url`), using the sandbox key from the ephemeral scratch env file, never committed. The inbound webhook signature path is verified by the fake/fixture tests locally and against the real webhook secret on deploy (production will use a mitos account). Next slices after this: A (allowlist gate + website Get Started flip + `/waitlist`), B (canonical-email anti-abuse), E (per-view DX polish).
