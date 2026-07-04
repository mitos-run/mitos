# Default Spend Cap Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** "Hard spend caps are on by default" becomes true: every org is enforced against a default hard cap even if it never set one, the cap is visible and adjustable in the console, and raising the cap (like topping up, which already works) lifts a spend_cap suspension.

**Architecture:** The enforcement chain already exists on main: the console drawdown driver calls `billing.Service.EnforceSpendCapFromLedger` per org each cycle; a breach suspends via `quota.BillingSuspender` into the Postgres `suspensions` table both gateway replicas read; a paid top-up lifts the suspension (reason-scoped `SuspensionLifter`). Three gaps remain. (1) `billing.Config` gains `DefaultCap`: when an org has NO `spend_caps` row, the default applies and is persisted as the org's row (so the console shows the cap being enforced); an explicit row always wins, including an explicitly uncapped `0/0` row. (2) Onboarding `Verify` seeds the default row at provisioning so new orgs see it immediately. (3) `handleSetSpendCap` lifts a `spend_cap` suspension after a cap change (idempotent; if still over cap the next drawdown cycle re-suspends).

**Tech Stack:** Go; `internal/saas/billing` (Money is int64 CENTS), `internal/saas/pgstore` (Postgres tests gated on `MITOS_TEST_DATABASE_DSN`, skip otherwise), `internal/saas/console`, `internal/saas/onboarding`, `cmd/console`.

## Global Constraints

- Never use em (U+2014) or en (U+2013) dashes anywhere: code, comments, commit messages, PR text. Connectors limited to `.` `,` `;` `:`.
- Error wrapping `fmt.Errorf("context: %w", err)`; octal literals `0o644`.
- Conventional commits with DCO: every commit via `git commit -s`.
- TDD: failing test first, same commit as the change.
- Secret values never logged; cap amounts and org ids are non-secret and fine.
- Lint gate: BOTH `golangci-lint run --timeout=5m` AND `GOOS=linux golangci-lint run --timeout=5m`.
- Working tree: `/Users/jannesstubbemann/repos/mitos-run/mitos/.claude/worktrees/hosted-offer-gaps` (main checkout; all files referenced exist there).
- Money semantics: `billing.Money` is integer cents (`internal/saas/billing/pricing.go:28`). The code default for the default hard cap is 500 cents ($5.00), env-overridable.

---

### Task 1: billing.Service DefaultCap fallback with persist-on-first-enforce

**Files:**
- Modify: `internal/saas/billing/service.go` (Config/Service/NewService ~lines 85-140; `EnforceSpendCap` ~line 333; `EnforceSpendCapFromLedger` ~line 404)
- Test: `internal/saas/billing/spendcap_ledger_test.go` (append)

**Interfaces:**
- Consumes: existing `SpendCapStore` (`Get` returns `(SpendCap, bool, error)`; `ok==false` means no row), `Suspender`, the `fixedNow`/`recordingSuspender`/`seedLedger` test fakes already in `service_test.go`/`spendcap_ledger_test.go`.
- Produces: `Config.DefaultCap Money` and `Service.capForOrg(ctx, orgID) (SpendCap, bool, error)`; Tasks 2 and 4 reference `DefaultCap`. Semantics later tasks rely on: no row + DefaultCap>0 means the default is enforced AND persisted as the org's row; an explicit row always wins, including explicit 0/0 (uncapped).

- [ ] **Step 1: Write the failing tests**

Append to `internal/saas/billing/spendcap_ledger_test.go` (reuse the file's existing helpers; if a helper named below does not exist, inline what the sibling tests in this file do):

```go
func TestEnforceSpendCapFromLedgerDefaultCapApplies(t *testing.T) {
	ledger := NewMemCreditLedger()
	caps := NewMemSpendCapStore()
	sus := &recordingSuspender{}
	svc := NewService(Config{Ledger: ledger, Caps: caps, Suspend: sus, DefaultCap: USD(5), Now: fixedNow})

	// No spend_caps row exists for the org. Burn past the $5 default.
	seedLedger(t, ledger, "org-default", []LedgerEntry{
		{OrgID: "org-default", Kind: KindTopUp, Amount: USD(5), At: fixedNow()},
		{OrgID: "org-default", Kind: KindUsageDrawdown, Amount: -USD(6), At: fixedNow()},
	})

	suspended, err := svc.EnforceSpendCapFromLedger(context.Background(), "org-default")
	if err != nil {
		t.Fatal(err)
	}
	if !suspended {
		t.Fatal("org with no explicit cap not suspended past the default cap")
	}
	if len(sus.calls) != 1 || sus.calls[0].reason != "spend_cap" {
		t.Fatalf("suspend calls = %+v, want one spend_cap suspension", sus.calls)
	}

	// The default was PERSISTED as the org's explicit row, so the console
	// billing view shows the cap that was enforced.
	got, ok, err := caps.Get(context.Background(), "org-default")
	if err != nil || !ok {
		t.Fatalf("default cap row not seeded: ok=%v err=%v", ok, err)
	}
	if got.HardCap != USD(5) {
		t.Fatalf("seeded hard cap = %d, want %d", got.HardCap, USD(5))
	}
}

func TestEnforceSpendCapFromLedgerExplicitUncappedRowWinsOverDefault(t *testing.T) {
	ledger := NewMemCreditLedger()
	caps := NewMemSpendCapStore()
	if err := caps.Set(context.Background(), SpendCap{OrgID: "org-uncapped"}); err != nil {
		t.Fatal(err)
	}
	sus := &recordingSuspender{}
	svc := NewService(Config{Ledger: ledger, Caps: caps, Suspend: sus, DefaultCap: USD(5), Now: fixedNow})

	seedLedger(t, ledger, "org-uncapped", []LedgerEntry{
		{OrgID: "org-uncapped", Kind: KindUsageDrawdown, Amount: -USD(50), At: fixedNow()},
	})

	suspended, err := svc.EnforceSpendCapFromLedger(context.Background(), "org-uncapped")
	if err != nil {
		t.Fatal(err)
	}
	if suspended || len(sus.calls) != 0 {
		t.Fatalf("explicit 0/0 (uncapped) row must win over the default; suspended=%v calls=%+v", suspended, sus.calls)
	}
}

func TestEnforceSpendCapFromLedgerExplicitHigherCapWinsOverDefault(t *testing.T) {
	ledger := NewMemCreditLedger()
	caps := NewMemSpendCapStore()
	if err := caps.Set(context.Background(), SpendCap{OrgID: "org-high", HardCap: USD(100)}); err != nil {
		t.Fatal(err)
	}
	sus := &recordingSuspender{}
	svc := NewService(Config{Ledger: ledger, Caps: caps, Suspend: sus, DefaultCap: USD(5), Now: fixedNow})

	seedLedger(t, ledger, "org-high", []LedgerEntry{
		{OrgID: "org-high", Kind: KindUsageDrawdown, Amount: -USD(10), At: fixedNow()},
	})

	suspended, err := svc.EnforceSpendCapFromLedger(context.Background(), "org-high")
	if err != nil {
		t.Fatal(err)
	}
	if suspended || len(sus.calls) != 0 {
		t.Fatalf("explicit higher cap must win over the default; suspended=%v calls=%+v", suspended, sus.calls)
	}
}
```

Note: `recordingSuspender.calls` field naming must match the existing fake in `service_test.go`; adapt the assertion field access, not the assertions.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/jannesstubbemann/repos/mitos-run/mitos/.claude/worktrees/hosted-offer-gaps && go test ./internal/saas/billing/ -run 'TestEnforceSpendCapFromLedger(DefaultCapApplies|ExplicitUncappedRowWinsOverDefault|ExplicitHigherCapWinsOverDefault)' -v`
Expected: `DefaultCapApplies` FAILS (compile error first: unknown field `DefaultCap`; then behaviorally: not suspended). The two explicit-row tests pass once it compiles; keep them, they pin the precedence.

- [ ] **Step 3: Implement**

In `internal/saas/billing/service.go`:

1. Add to `Config` and `Service` (mirroring existing fields):

```go
	// DefaultCap, when positive, is the hard spend cap enforced for an org
	// with NO spend_caps row ("hard spend caps are on by default"). On first
	// enforcement it is persisted as the org's row so the console shows the
	// cap being enforced. An explicit row always wins, including an
	// explicitly uncapped 0/0 row: clearing the cap is the documented
	// escape hatch. Cents.
	DefaultCap Money
```

Carry it in `NewService` (`defaultCap: cfg.DefaultCap`).

2. Add the resolver:

```go
// capForOrg resolves the org's effective spend cap. The explicit row always
// wins (including an explicitly uncapped 0/0 row). With no row and a
// configured DefaultCap, the default applies and is persisted as the org's
// row, so the enforced cap is the visible cap.
func (s *Service) capForOrg(ctx context.Context, orgID string) (SpendCap, bool, error) {
	cap, ok, err := s.caps.Get(ctx, orgID)
	if err != nil {
		return SpendCap{}, false, fmt.Errorf("get spend cap: %w", err)
	}
	if ok || s.defaultCap <= 0 {
		return cap, ok, nil
	}
	cap = SpendCap{OrgID: orgID, HardCap: s.defaultCap}
	if err := s.caps.Set(ctx, cap); err != nil {
		return SpendCap{}, false, fmt.Errorf("seed default spend cap: %w", err)
	}
	return cap, true, nil
}
```

3. In `EnforceSpendCap`, replace the opening

```go
	cap, ok, err := s.caps.Get(ctx, orgID)
	if err != nil {
		return false, fmt.Errorf("get spend cap: %w", err)
	}
	if !ok {
		return false, nil
	}
```

with

```go
	cap, ok, err := s.capForOrg(ctx, orgID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
```

4. In `EnforceSpendCapFromLedger`, replace its opening `s.caps.Get` block the same way (keep the `if !ok || (cap.HardCap <= 0 && cap.SoftCap <= 0) { return false, nil }` short-circuit; with a seeded default row `ok` is true and `HardCap > 0`, so enforcement proceeds).

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/saas/billing/ -v`
Expected: all PASS, including the pre-existing `TestEnforceSpendCapFromLedgerNoCapIsNoOp` (it constructs the service WITHOUT `DefaultCap`, so the uncapped short-circuit still holds; if it fails, `capForOrg` is seeding when `defaultCap == 0`, which is a bug in your change).

- [ ] **Step 5: Commit**

```bash
git add internal/saas/billing/service.go internal/saas/billing/spendcap_ledger_test.go
git commit -s -m "feat(billing): default hard spend cap enforced and persisted when an org has no explicit cap (#615)"
```

---

### Task 2: Onboarding seeds the default cap row at Verify

**Files:**
- Modify: `internal/saas/onboarding/service.go` (fields ~line 334, options ~line 355, `NewService` ~line 412, `Verify` credit-grant block ~line 652)
- Test: the existing Verify success-path test in `internal/saas/onboarding/` (find with `grep -rn "GrantSignupCredit\|signup credit" internal/saas/onboarding/*_test.go`; extend the test that asserts the credit grant)

**Interfaces:**
- Consumes: `billing.SpendCapStore` and `billing.SpendCap` from Task 1's semantics (explicit row wins); the existing option pattern (`WithSignupCredit`).
- Produces: `onboarding.WithSpendCapSeed(caps billing.SpendCapStore, hardCap billing.Money)` option; Task 4 wires it in `cmd/console`.

- [ ] **Step 1: Write the failing test**

Extend the existing Verify success test: construct the service with the new option and assert the row after Verify.

```go
	caps := billing.NewMemSpendCapStore()
	// ... existing service construction gains:
	//     onboarding.WithSpendCapSeed(caps, billing.USD(5)),

	// ... after the existing successful Verify assertions:
	seeded, ok, err := caps.Get(context.Background(), res.OrgID) // use the org id the test already asserts on
	if err != nil || !ok {
		t.Fatalf("default spend cap not seeded at verify: ok=%v err=%v", ok, err)
	}
	if seeded.HardCap != billing.USD(5) {
		t.Fatalf("seeded hard cap = %d, want %d", seeded.HardCap, billing.USD(5))
	}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/saas/onboarding/ -run <NameOfExtendedTest> -v`
Expected: FAIL (compile: undefined `WithSpendCapSeed`).

- [ ] **Step 3: Implement**

In `internal/saas/onboarding/service.go`:

1. Service fields (next to `credit`):

```go
	spendCaps      billing.SpendCapStore
	defaultHardCap billing.Money
```

2. Option (next to `WithSignupCredit`):

```go
// WithSpendCapSeed makes Verify seed the org's default hard spend cap as an
// explicit spend_caps row at provisioning, so the cap is visible and
// adjustable in the console from the first sign-in ("hard spend caps are on
// by default").
func WithSpendCapSeed(caps billing.SpendCapStore, hardCap billing.Money) Option {
	return func(s *Service) {
		s.spendCaps = caps
		s.defaultHardCap = hardCap
	}
}
```

3. In `Verify`, immediately after the `GrantSignupCredit` block (~line 656):

```go
	// Seed the default hard spend cap as the org's explicit row. Idempotent
	// on retry: the upsert rewrites the same default, and MarkVerified below
	// blocks a re-run after a user could have changed it.
	if s.spendCaps != nil && s.defaultHardCap > 0 {
		if err := s.spendCaps.Set(ctx, billing.SpendCap{OrgID: org.ID, HardCap: s.defaultHardCap}); err != nil {
			return VerifyResult{}, fmt.Errorf("onboarding verify: seed default spend cap: %w", err)
		}
	}
```

(Match the surrounding code's variable names for the org; the credit grant right above uses the same org id.)

- [ ] **Step 4: Run the package tests**

Run: `go test ./internal/saas/onboarding/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/saas/onboarding/service.go internal/saas/onboarding/<extended_test_file>
git commit -s -m "feat(onboarding): seed the default hard spend cap row at verify (#615)"
```

---

### Task 3: Cap change lifts a spend_cap suspension

**Files:**
- Modify: `internal/saas/console/spendcap.go` (`handleSetSpendCap`), plus the `BillingReader`/`Deps` struct that carries `Caps`/`Status`/`Ledger`/`Rates` (find it: `grep -n "Caps" internal/saas/console/console.go internal/saas/console/*.go`)
- Test: the existing handler test for spend cap (find: `grep -rln "spend-cap\|SetSpendCap" internal/saas/console/*_test.go`; append)

**Interfaces:**
- Consumes: `billing.SuspensionLifter` (exists: `LiftReason(ctx, orgID, reason) (bool, error)`, satisfied by `quota.BillingSuspender`), `billing.StatusStore`.
- Produces: a `Lifter billing.SuspensionLifter` field on the console billing deps struct; Task 4 wires it.

- [ ] **Step 1: Write the failing test**

Append to the console spend-cap handler test file, modeling request construction and authorization on the existing set-spend-cap test in the same file:

```go
type recordingLifter struct {
	calls []string
	lift  bool
}

func (l *recordingLifter) LiftReason(_ context.Context, orgID, reason string) (bool, error) {
	l.calls = append(l.calls, orgID+"/"+reason)
	return l.lift, nil
}

func TestSetSpendCapLiftsSpendCapSuspension(t *testing.T) {
	// Fixture: same console + authorized org as the existing set-spend-cap
	// test, plus:
	lifter := &recordingLifter{lift: true}
	status := billing.NewMemStatusStore()
	_ = status.SetStatus(context.Background(), orgID, billing.StatusSuspended)
	// wire lifter + status into the console deps the same way Caps is wired.

	// POST /console/billing/spend-cap with {"soft_cents":0,"hard_cents":2000}
	// (reuse the existing test's request helper), expect 200.

	if len(lifter.calls) != 1 || lifter.calls[0] != orgID+"/spend_cap" {
		t.Fatalf("lift calls = %v, want one spend_cap lift for %s", lifter.calls, orgID)
	}
	st, err := status.Status(context.Background(), orgID)
	if err != nil {
		t.Fatal(err)
	}
	if st != billing.StatusActive {
		t.Fatalf("status after lifted cap change = %q, want active", st)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/saas/console/ -run TestSetSpendCapLiftsSpendCapSuspension -v`
Expected: FAIL (compile: no `Lifter` field).

- [ ] **Step 3: Implement**

1. Add to the console billing deps struct (exact struct found in Step 1's grep; it already has `Caps` and `Status`):

```go
	// Lifter lifts a reason-scoped suspension; the spend-cap handler uses it
	// so a cap change recovers a spend_cap-suspended org. Optional.
	Lifter billing.SuspensionLifter
```

2. In `handleSetSpendCap`, after the successful `Caps.Set` and before the final `writeJSON`:

```go
	// A cap change is the user's explicit continue decision: lift a
	// spend_cap suspension (reason-scoped, never a manual hold) and restore
	// active status. Idempotent; if the org is still over the new cap the
	// next drawdown cycle re-suspends.
	if c.deps.Billing.Lifter != nil {
		lifted, err := c.deps.Billing.Lifter.LiftReason(r.Context(), orgID, "spend_cap")
		if err != nil {
			c.deps.Log.Warn("spend cap change: lift suspension", "org", orgID, "err", err.Error())
		} else if lifted && c.deps.Billing.Status != nil {
			if err := c.deps.Billing.Status.SetStatus(r.Context(), orgID, billing.StatusActive); err != nil {
				c.deps.Log.Warn("spend cap change: restore active status", "org", orgID, "err", err.Error())
			}
		}
	}
```

(Adapt `c.deps.Billing.X` to the actual field paths used elsewhere in the handler; the existing code accesses `c.deps.Billing.Caps`.)

- [ ] **Step 4: Run the package tests**

Run: `go test ./internal/saas/console/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/saas/console/
git commit -s -m "feat(console): a spend cap change lifts a spend_cap suspension and restores active status (#615)"
```

---

### Task 4: Wire DefaultCap through cmd/console + docs + lint

**Files:**
- Modify: `cmd/console/main.go` (drawdown service Config ~line 270; onboarding construction, which lives in `cmd/console/onboarding.go` ~line 103; console deps wiring ~lines 203-208)
- Modify: docs (find the page: `grep -rln "spend cap" docs/ | grep -v superpowers`)

**Interfaces:**
- Consumes: `billing.Config.DefaultCap` (Task 1), `onboarding.WithSpendCapSeed` (Task 2), console `Lifter` field (Task 3), the already-wired `billingSuspender` (satisfies `billing.SuspensionLifter`) and `spendCapStore`.
- Produces: env knob `MITOS_CONSOLE_DEFAULT_SPEND_CAP_CENTS` (default `500`, `0` disables).

- [ ] **Step 1: Parse the env knob**

In `cmd/console/main.go`, next to the `MITOS_CONSOLE_RATES` parsing:

```go
	// Default hard spend cap in cents for orgs with no explicit cap ("hard
	// spend caps are on by default"). 0 disables the default (dev escape
	// hatch); the code default matches the $5 signup credit.
	defaultCapCents := int64(500)
	if v := os.Getenv("MITOS_CONSOLE_DEFAULT_SPEND_CAP_CENTS"); v != "" {
		defaultCapCents, err = strconv.ParseInt(v, 10, 64)
		if err != nil || defaultCapCents < 0 {
			log.Fatalf("MITOS_CONSOLE_DEFAULT_SPEND_CAP_CENTS: invalid value %q", v)
		}
	}
	defaultCap := billing.Money(defaultCapCents)
```

Hoist `spendCapStore` construction ABOVE this if it currently sits below the onboarding wiring; both the drawdown service and onboarding need it.

- [ ] **Step 2: Thread it**

1. Drawdown service: add `DefaultCap: defaultCap` to the existing `billing.NewService(billing.Config{...})` at ~line 270.
2. Onboarding: add `onboarding.WithSpendCapSeed(spendCapStore, defaultCap)` to the options list where the onboarding service is built (`cmd/console/onboarding.go`; pass `spendCapStore` and `defaultCap` in from `main.go` if the builder does not already see them; follow how the signup-credit option is plumbed).
3. Console deps: set `Lifter: billingSuspender` next to where `Caps`/`Status` are set in the console wiring block (~lines 203-208).
4. Guard: only skip the seed when `defaultCap == 0` (the option and `capForOrg` both already treat 0 as off).

- [ ] **Step 3: Build and test everything**

Run: `go build ./... && go test ./internal/saas/... ./cmd/console/...`
Expected: PASS.

- [ ] **Step 4: Update docs**

In the docs page found by the grep (the hosted billing/spend-cap doc), document: the default hard cap ($5, env `MITOS_CONSOLE_DEFAULT_SPEND_CAP_CENTS`, 0 disables), that the default is persisted as the org's visible cap on first enforcement or at signup, that an explicit cap (including clearing to 0/0) always wins, and that a cap change or paid top-up lifts a spend_cap suspension while manual-hold suspensions are never auto-lifted. Check for em/en dashes.

- [ ] **Step 5: Lint both ways**

Run: `golangci-lint run --timeout=5m && GOOS=linux golangci-lint run --timeout=5m`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add cmd/console/ docs/
git commit -s -m "feat(console): wire the default spend cap end to end; caps are on by default (#615)"
```

---

## Deployment note (outside this repo)

The prod rollout sets `MITOS_CONSOLE_DEFAULT_SPEND_CAP_CENTS` explicitly in the mono gitops repo (console env) to match the configured signup credit; the code default of 500 covers it if unset. Not part of this repo's PR.
