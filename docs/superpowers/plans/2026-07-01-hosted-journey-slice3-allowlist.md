# Hosted journey slice 3: allowlist gate + waitlist (Workstream A)

> For agentic workers: execute with superpowers:subagent-driven-development, one task at a time, task-reviewed, then a slice final review. Steps are checkbox-tracked.

**Goal:** Gate the console self-serve signup behind an allowlist so Get Started can go live to invited users only: an allowed email provisions and gets the signup credit at verify; a not-allowed email lands on the waitlist with a calm "you are on the list" state and no provisioning; an internal approve-signup adds the allowlist row and sends a "you are in" email.

**Architecture:** Extend the existing `internal/saas/onboarding` funnel (SignUp -> verify -> provision) and the console BFF. Add a new `allowlist` store (in-memory + Postgres behind one interface, matching the pgstore pattern). The verify path consults `IsAllowed(canonical email)` before provisioning. Approval is an internal, non-self-serve console endpoint plus a cluster-ops-style invocation. One console image serves self-host and hosted; the allowlist gate is hosted-only and capability/flag gated so a self-hoster is unaffected.

**Tech stack:** Go BFF + pgx/pgxpool, the onboarding service, the `EmailSender` seam, the console router; React/TS SPA for the verify/waitlist state.

## Global Constraints (every task inherits these)

- No em or en dashes anywhere (code, comments, SQL, SPA copy, commit messages). ASCII hyphen only.
- The email address and the verify token are NEVER logged (hash for events); allowlist notes are non-secret but the email is PII, so events key on the email hash.
- Canonical email is the identity: `IsAllowed`, the allowlist row, and the abuse checks all operate on the canonicalized address (the existing normalizer in `internal/saas/onboarding/http.go`), so `foo+x@gmail` and `f.o.o@googlemail` map to one identity.
- Money is integer cents; the signup credit is granted at most once per canonical identity (unchanged from today, just gated by allow).
- Self-host cleanliness: the allowlist gate and approve-signup are hosted-only; when the signup flag is off / community edition, none of these surfaces appear and the existing product is unchanged.
- Go: `fmt.Errorf("ctx: %w", err)`; gofmt + both golangci-lint invocations (darwin AND `GOOS=linux`) clean. TDD: failing test first, same commit. DCO `git commit -s`. Conventional commits. Stage explicit paths only.

## What already exists (do not rebuild)

- `onboarding.Service`: `SignUp(ctx, email, useCase)` (ModeWaitlist records a waitlist entry and provisions nothing; ModeOpen creates a pending signup, emails a verify token), `Verify(ctx, rawToken)` (validates token, provisions account + Personal org + signup credit + first key, idempotent). `SignupResult{Waitlisted bool, PendingID string}`, `VerifyResult`.
- The pending store already has `AddWaitlist(ctx, WaitlistEntry)` and `PutPending`/`GetPendingByTokenHash`. `PendingSignup` carries `Email`, `TokenHash`, `Verified`, `UseCase`.
- Canonical-email normalization lives in `internal/saas/onboarding/http.go` (lowercasing + address parse; see the no-enumeration comment near line 207).
- The `EmailSender` seam sends the verification email (`SendVerification`); the SMTP + no-op implementations live in `internal/saas/onboarding/smtp.go`.
- pgstore pattern (creditledger.go / spendcapstore.go from slice D) + migrations `//go:embed migrations/*.sql`; highest is `0004`.
- Signup flag `MITOS_CONSOLE_SIGNUP` (on => ModeOpen) and `MITOS_CONSOLE_SIGNUP_CREDIT_CENTS`.

## Out of scope for this slice (tracked separately)

- The `mitos-run/website` `SIGNUP_BASE` flip + `/waitlist` marketing page + use-cases nav (Workstream A website half): lives in the SEPARATE `mitos-run/website` repo, which must be added to the session before it can be done. This plan covers the console/onboarding backend + the SPA verify/waitlist state only.
- Workstream B (Gmail folding beyond the current normalizer, disposable blocklist, IP velocity, Friendly Captcha): its own slice.
- Populating the org<->Paddle customer map (a slice-D follow-up).

---

### Task A1: allowlist store (interface + in-memory + Postgres + migration)

**Files:**
- Create: `internal/saas/onboarding/allowlist.go` (the `Allowlist` interface + `MemAllowlist` + `IsAllowed` precedence with auto-allow domains)
- Create: `internal/saas/onboarding/allowlist_test.go`
- Create: `internal/saas/pgstore/allowlist.go` (`PgAllowlist`)
- Create: `internal/saas/pgstore/allowlist_test.go`
- Create: `internal/saas/pgstore/migrations/0005_allowlist.sql`

**Interfaces:**
- Produces:
  ```go
  type Allowlist interface {
      // IsAllowed reports whether a canonical email may provision. Precedence:
      // an auto-allow domain match, else an allowlist row for the exact canonical
      // email. Never logs the email.
      IsAllowed(ctx context.Context, canonicalEmail string) (bool, error)
      // Add inserts (idempotently) an allowlist row for a canonical email.
      Add(ctx context.Context, canonicalEmail, note string, now time.Time) error
  }
  ```
  Auto-allow domains come from a constructor argument (the caller reads `MITOS_CONSOLE_AUTOALLOW_DOMAINS`, default `mitos.run`, comma-split, lowercased). `MemAllowlist` and `PgAllowlist` both satisfy it.
- Schema `0005_allowlist.sql`: `allowlist (email TEXT PRIMARY KEY, note TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL)`. `Add` upserts (ON CONFLICT (email) DO NOTHING). In-table means approved; no status enum.

**Acceptance:** table tests for `IsAllowed` precedence (auto-allow domain hit; exact-row hit; neither -> false; domain match is case-insensitive and canonical); `Add` idempotency; the pg round-trip mirrors the creditledger/spendcap pg harness (skips cleanly without a DB but compiles).

### Task A2: verify gate consults the allowlist

**Files:**
- Modify: `internal/saas/onboarding/service.go` (`Verify` checks `IsAllowed` before provisioning; a not-allowed pending is marked waitlisted and returns a waitlisted result, no org/credit/key)
- Modify: the `Service` struct + its constructor/options to hold the `Allowlist` (default a permissive/nil that allows all, so self-host/community is unchanged when no allowlist is configured)
- Modify: `internal/saas/onboarding/service_test.go`

**Interfaces:**
- Consumes A1's `Allowlist.IsAllowed`.
- Produces: `VerifyResult` gains `Waitlisted bool` (when true, Account/Org/FirstKey are zero and no credit is granted). A `WithAllowlist(Allowlist)` option; when unset, verify behaves as today (allow all) so community edition is unaffected.

**Acceptance:** with an allowlist configured, a token whose canonical email is allowed provisions + grants credit + issues the key (existing behavior); a token whose email is NOT allowed returns `VerifyResult{Waitlisted: true}`, provisions nothing, grants no credit, issues no key, and marks the pending record waitlisted (so a later approve + re-verify can provision); with NO allowlist configured, behavior is exactly as today. The email/token are never logged.

### Task A3: internal approve-signup endpoint + ops invocation

**Files:**
- Create: `internal/saas/console/approvesignup.go` (`POST /internal/approve-signup {email, note?}` -> inserts the allowlist row via A1 `Add` (canonicalized) and sends the approved email; idempotent)
- Modify: `internal/saas/console/console.go` (mount the internal route; hold the Allowlist + EmailSender deps) and `cmd/console/main.go` (wire them)
- Create: `internal/saas/console/approvesignup_test.go`
- A repo ops entry to invoke it (a `scripts/` helper or a documented curl), matching the paperclip approve-signup ergonomics; approval is NOT self-serve.

**Interfaces:**
- Consumes A1 `Allowlist.Add` and the `EmailSender` seam (A4 template).
- Produces: `POST /internal/approve-signup` returns 200 `{email}` on success (idempotent re-approve is still 200), 400 on a missing/malformed email. This endpoint is INTERNAL (same protection as the other `/internal/*` machine endpoints); it is never exposed to tenants.

**Acceptance:** approving a new email inserts the row and sends one approved email; re-approving the same email is idempotent (still 200, no duplicate row error, at most one email per call); a malformed email is 400; the email/token are never logged; the endpoint is not reachable on the tenant-facing surface.

### Task A4: approved-email template on the EmailSender seam

**Files:**
- Modify: `internal/saas/onboarding/smtp.go` (add `SendApproved(ctx, email)` to the `EmailSender` seam + the SMTP and no-op implementations)
- Modify: `internal/saas/onboarding/smtp_test.go`

**Interfaces:**
- Produces: `EmailSender.SendApproved(ctx, email)` sends the "you are in, sign in to run your first fork" email, on-brand, no dashes. The no-op implementation is a zero-cost call (self-host/dev). No "you joined" email exists (silent join, matching paperclip).

**Acceptance:** the SMTP implementation composes an approved message to the address with the correct subject/body (asserted against the fake SMTP in smtp_test.go); the no-op returns nil without sending; no email or token is logged.

### Task A5: SPA verify/waitlist state (calm "you are on the list", no dead end)

**Files:**
- Modify: the SPA verify view/route under `web/app/src` that handles the post-verify redirect (find it: the page hit after clicking the verify link) to render a calm waitlisted state when verify returns waitlisted (a `?state=waitlisted` or the API's waitlisted response), with links to docs and sign out; no dead end.
- Test: the corresponding `*.test.tsx`.

**Interfaces:**
- Consumes A2's waitlisted verify outcome (surfaced through the console verify endpoint's response/redirect).

**Acceptance:** when verify yields waitlisted, the SPA shows the on-brand "you are on the list" state (Fluorescence tokens, `@mitos/brand`, no dashes) with a docs link and a sign-out affordance, and never a blank or error dead end; when verify succeeds, the existing first-run path is unchanged. Test asserts both branches render.

---

## Sequencing

A1 (store) first; A2 (verify gate) depends on A1; A4 (email) before A3 (which sends it); A3 depends on A1 + A4; A5 (SPA) depends on A2's waitlisted outcome being observable. Then a slice-A final whole-branch review. The website flip + `/waitlist` marketing page and the deploy flag flip (`MITOS_CONSOLE_SIGNUP=1`, auto-allow `mitos.run`, $5 credit) are the enablement step, gated on the `mitos-run/website` repo being added and a deploy window.
