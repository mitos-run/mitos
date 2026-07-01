# Workstream 4: Paddle billing UI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Turn the $5 trial into a paying relationship: a working billing view (ledger that actually renders), spend-cap controls the user can set, and an upgrade path that opens a Paddle checkout. Paddle is the Merchant-of-Record, so card capture and management live in Paddle's hosted checkout/portal (we link, we do not build a card form).

**Architecture:** Extend the existing provider-neutral billing seam. Paddle is already the primary provider (webhook verify + portal URL). Add a checkout-session URL to the Paddle provider, a console endpoint to start an upgrade, a console endpoint to set a spend cap, and the SPA UI for both. Fix the shipped ledger-serialization bug so the billing ledger renders. The actual Paddle products/plans (box, packaged) and box reserved-pool provisioning are external/cluster follow-ups, flagged not built here.

**Tech Stack:** Go (`internal/saas/billing`, `internal/saas/billingprovider/paddle`, `internal/saas/console`, `cmd/console`), React 18 + TanStack Query (web/app), Vitest, `@mitos/brand`.

## Global Constraints

- No em (U+2014) or en (U+2013) dashes anywhere. ASCII hyphen only.
- Go: `fmt.Errorf("ctx: %w", err)`; gofmt + `go vet` clean. Provider API keys, webhook secrets, and any payment detail are NEVER logged, NEVER in errors, NEVER in conditions. Ids and counts only.
- Money is integer cents (`billing.Money`); never float for accounting.
- Fluorescence tokens only in SPA chrome; `@mitos/brand` components; approved numbers only; brand voice.
- DCO: every commit `git commit -s`. Conventional prefixes. Stage explicit paths only.
- Work in the ISOLATED WORKTREE /Users/jannesstubbemann/repos/mitos-run/mitos-ws2 (branch hosted-launch-ws4). Go from the worktree root; SPA from web/app (`pnpm test`, `pnpm build`). A test Postgres is at `postgres://postgres:test@127.0.0.1:55432/mitos_fresh?sslmode=disable` if needed (recreate fresh; pgstore tests Open-before-truncate).
- Mirror existing patterns: `internal/saas/console/portal.go` (the billing.manage-gated billing endpoint pattern), `internal/saas/billingprovider/paddle/paddle.go` + `paddle_test.go` (the provider + its fake/test pattern), `web/app/src/views/Billing.tsx` + `web/app/src/data/account.ts`.

---

### Task 1: Fix the billing ledger serialization (shipped bug)

`GET /console/billing` returns `LedgerEntries []billing.LedgerEntry` with no JSON tags, so it serializes PascalCase (`At`, `Amount`, `Note`), but the SPA expects `{ts, cents, reason}`. The ledger table renders every row as dashes. Fix with an explicit view model.

**Files:**
- Modify: `internal/saas/console/console.go` (the `BillingView` + the `/console/billing` handler: map `LedgerEntry` to a snake_case view struct)
- Test: `internal/saas/console/console_test.go` (assert the JSON ledger entry field names)
- Verify: `web/app/src/views/Billing.tsx` already reads `{ts, cents, reason}` (no SPA change needed; confirm).

**Interfaces:**
- Produces: a `ledgerEntryView` (or similar) with `Ts time.Time json:"ts"`, `Cents int64 json:"cents"`, `Reason string json:"reason"`, used in `BillingView.LedgerEntries`.

- [ ] **Step 1: Write the failing test**

In `console_test.go`, add a test that creates an org with a ledger entry, calls the `/console/billing` handler, decodes the JSON, and asserts `ledger_entries[0]` has the keys `ts`, `cents`, `reason` with the right values (cents = the entry Amount, reason = the entry Note). Mirror the existing billing handler test setup.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/saas/console/ -run Billing -v`
Expected: FAIL (current JSON has PascalCase keys).

- [ ] **Step 3: Implement the view mapping**

In `console.go`, change `BillingView.LedgerEntries` to `[]ledgerEntryView` where `ledgerEntryView` has json tags `ts`, `cents`, `reason`. In the handler, map each `billing.LedgerEntry` to `ledgerEntryView{Ts: e.At, Cents: int64(e.Amount), Reason: e.Note}`. Keep the rest of `BillingView` unchanged.

- [ ] **Step 4: Run to verify it passes, confirm SPA contract, commit**

Run: `go test ./internal/saas/console/ -run Billing -v && go build ./... && go vet ./internal/saas/console/`
Expected: PASS, clean. Confirm `web/app/src/api.ts` BillingView ledger_entries type already matches `{ts, cents, reason}` (it does); run `cd web/app && pnpm test` to confirm no SPA regression.

```bash
git add internal/saas/console/console.go internal/saas/console/console_test.go
git commit -s -m "fix(console): billing ledger entries serialize as ts/cents/reason for the SPA"
```

---

### Task 2: Spend-cap controls (set soft/hard caps from the console)

A console endpoint to set the org spend cap (billing.manage gated) and the SPA UI to set it. This is the user-facing guardrail that makes "instant" safe.

**Files:**
- Create: `internal/saas/console/spendcap.go` (the `POST /console/billing/spend-cap` handler)
- Modify: `internal/saas/console/console.go` (mount the route) and the console deps (a `SpendCapWriter` seam wrapping `billing.Service.SetSpendCap` or the `SpendCapStore`)
- Test: `internal/saas/console/spendcap_test.go`
- Modify: `web/app/src/views/Billing.tsx` (a "Set spend cap" form), `web/app/src/data/account.ts` (a `useSetSpendCap` mutation), `web/app/src/api.ts` (the post)
- Test: `web/app/src/views/Billing.test.tsx`

**Interfaces:**
- Consumes: `saas.PermManageBilling` (the gate), a way to persist the cap (the existing `billing.SpendCapStore` / `billing.Service.SetSpendCap`).
- Produces: `POST /console/billing/spend-cap` body `{ soft_cents: number, hard_cents: number }` -> 200 (or 403 without billing.manage); the SPA mutation + form.

- [ ] **Step 1: Write the failing Go test**

`spendcap_test.go`: a caller WITH billing.manage can POST `{soft_cents, hard_cents}` and the cap is persisted (read back via the billing view shows the new caps); a caller WITHOUT billing.manage gets 403; invalid input (negative, soft > hard) is rejected 400. Mirror `portal_test.go` for the permission-gated setup.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/saas/console/ -run SpendCap -v` (FAIL).

- [ ] **Step 3: Implement the endpoint**

Create `spendcap.go`: decode `{soft_cents, hard_cents}`, validate (non-negative, hard >= soft when both > 0), require `saas.PermManageBilling` (mirror `portal.go`), call the cap writer (`SetSpendCap` with `billing.SpendCap{OrgID, SoftCap: Money(soft), HardCap: Money(hard)}`), return 200. Never log amounts as anything but the org id + counts. Mount `POST /console/billing/spend-cap` in console.go next to the portal route. Wire the cap writer in `cmd/console`.

- [ ] **Step 4: Run the Go test, then the SPA**

Run: `go test ./internal/saas/console/ -run SpendCap -v && go build ./... && go vet ./internal/saas/console/`
Expected: PASS, clean.

- [ ] **Step 5: Implement the SPA**

In `api.ts` add `setSpendCap(soft, hard)` (POST /console/billing/spend-cap). In `account.ts` add `useSetSpendCap()` (a mutation invalidating the billing query). In `Billing.tsx` add a "Set spend cap" form (two money inputs soft/hard, a Save button) gated on the caller having billing.manage (read from capabilities/role if available; otherwise show it and let the 403 surface a clear message). On success show a calm confirmation (aria-live). Tokens only, @mitos/brand inputs/Button, magenta focus ring, no dashes. Update `Billing.test.tsx` to assert the form posts the caps and shows confirmation.

- [ ] **Step 6: Run SPA tests + build, commit**

Run: `cd web/app && pnpm test && pnpm build`
Expected: PASS, clean.

```bash
git add internal/saas/console/spendcap.go internal/saas/console/console.go internal/saas/console/spendcap_test.go cmd/console/ web/app/src/views/Billing.tsx web/app/src/views/Billing.test.tsx web/app/src/data/account.ts web/app/src/api.ts
git commit -s -m "feat(console): set org spend cap from the billing view (billing.manage gated)"
```

---

### Task 3: Paddle checkout / upgrade link

For a Merchant-of-Record, upgrading to a paid plan (box or packaged) means opening a Paddle hosted checkout for that plan. Add a checkout-URL to the Paddle provider, a console endpoint that returns it, and an SPA "Upgrade" affordance. The actual Paddle products/plan ids come from config; defining them in Paddle and box provisioning are external follow-ups.

**Files:**
- Modify: `internal/saas/billingprovider/paddle/paddle.go` (add `CheckoutURL(ctx, customerRef, planRef) (string, error)` calling the Paddle Billing API to create a transaction/checkout for the plan)
- Test: `internal/saas/billingprovider/paddle/paddle_test.go` (fake the Paddle API; assert the checkout call and parsed URL; assert no secret logged)
- Create: `internal/saas/console/checkout.go` (`GET /console/billing/checkout?plan=<id>` -> `{url}`, billing.manage gated; maps the plan key to the configured Paddle plan ref)
- Modify: `internal/saas/console/console.go` (mount) + `cmd/console/billing.go` (wire the checkout linker + a plan-key->plan-ref config map)
- Test: `internal/saas/console/checkout_test.go`
- Modify: `web/app/src/views/Billing.tsx` (Upgrade buttons: "Reserve a box", "Buy an app" that open the checkout URL), `account.ts`/`api.ts`
- Test: `Billing.test.tsx`

**Interfaces:**
- Consumes: the Paddle `Config` (api key), the customer mapping (`OrgCustomers`), a plan-key -> Paddle plan-ref config map.
- Produces: `Paddle.CheckoutURL(...)`; `GET /console/billing/checkout?plan=<key>` -> `{url}`; the SPA upgrade buttons.

- [ ] **Step 1: Write the failing provider test**

In `paddle_test.go`, add a test with an `httptest.Server` standing in for the Paddle Billing API (asserts the bearer auth and the plan/customer in the request body, returns a checkout/transaction with a hosted URL). Assert `CheckoutURL` returns that URL, that a non-2xx maps to a wrapped error, and that NO api key appears in any error.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/saas/billingprovider/paddle/ -run Checkout -v` (FAIL).

- [ ] **Step 3: Implement CheckoutURL**

Add `CheckoutURL(ctx, customerRef, planRef)` to the Paddle provider: POST to the Paddle Billing API (the appropriate endpoint to create a checkout/transaction for a price/plan and customer), parse the hosted-checkout URL from the response, return it. Bearer api key in the Authorization header; never log the key or the response secrets. Short timeout. Mirror the `PortalURL` implementation style.

- [ ] **Step 4: Write + implement the console endpoint**

`checkout_test.go`: a billing.manage caller GETs `/console/billing/checkout?plan=box` and gets a `{url}`; an unknown plan key -> 400; without billing.manage -> 403; no customer mapping -> 404 (mirror portal.go). Implement `checkout.go`: resolve the org customer ref, map the `plan` key to the configured Paddle plan ref (config map in cmd/console; unknown key -> 400), call `provider.CheckoutURL`, return `{url}`. Mount the route. Wire the plan-key map + the checkout linker in `cmd/console/billing.go` (the plan refs come from `MITOS_CONSOLE_PADDLE_PLAN_*` env / values; empty disables the upgrade affordance).

- [ ] **Step 5: Run Go tests, build, vet**

Run: `go test ./internal/saas/billingprovider/paddle/ ./internal/saas/console/ -v && go build ./... && go vet ./internal/saas/...`
Expected: PASS, clean.

- [ ] **Step 6: SPA upgrade affordance**

In `api.ts` add `checkoutUrl(plan)`; in `account.ts` add a helper. In `Billing.tsx` add an "Upgrade" area with "Reserve a box" and "Buy an app" buttons that fetch the checkout URL and `window.open` it (mirror `onManageBilling`). Show the buttons only when the capabilities indicate billing is enabled. On error show a calm message. Update `Billing.test.tsx`. Tokens only, no dashes.

- [ ] **Step 7: Run SPA tests + build, commit**

Run: `cd web/app && pnpm test && pnpm build`
Expected: PASS, clean.

```bash
git add internal/saas/billingprovider/paddle/ internal/saas/console/checkout.go internal/saas/console/console.go internal/saas/console/checkout_test.go cmd/console/ web/app/src/views/Billing.tsx web/app/src/views/Billing.test.tsx web/app/src/data/account.ts web/app/src/api.ts
git commit -s -m "feat(billing): Paddle checkout link for box and packaged upgrades"
```

---

## Self-Review

**1. Spec coverage (WS4 billing UI):** the ledger renders (Task 1), the user can set spend caps (Task 2), and can upgrade to box/packaged via a Paddle hosted checkout (Task 3). Card capture and management are intentionally NOT built (Paddle MoR handles them in its hosted checkout/portal, already linked via `/console/billing/portal`). The actual Paddle products/plan definitions, the box reserved-pool provisioning, the durable customer/cap/ledger stores, and the real Paddle webhook wiring are external/cluster follow-ups (flagged), as is the marketing pricing-page IA (separate website repo).

**2. Placeholder scan:** each task names the exact endpoints, the view-model field tags, the permission gate (`PermManageBilling`), and mirrors named existing files (`portal.go`, `paddle.go`). The Paddle checkout endpoint specifics depend on the Paddle Billing API shape; the task says to mirror `PortalURL` and verify against a fake server, which is the right local-verification boundary (the live Paddle checkout is verified with a real sandbox account, not here).

**3. Consistency:** `ledgerEntryView{ts,cents,reason}` (Task 1) matches the SPA `api.ts` BillingView type. `POST /console/billing/spend-cap` (Task 2) and `GET /console/billing/checkout` (Task 3) both sit beside `/console/billing/portal` and reuse its billing.manage gate. The plan-key map in Task 3 is the single source for the SPA upgrade buttons and the console endpoint.

Note for the executor: all three tasks are locally verifiable (Go tests + fake Paddle server + vitest). The live Paddle checkout/webhook round trip needs a Paddle sandbox account and is verified on deploy, not locally. Money stays integer cents throughout; never log provider keys or amounts beyond org-id/count.
