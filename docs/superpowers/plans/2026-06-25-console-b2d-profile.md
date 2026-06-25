# Console B2d: user profile + account settings Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`). Do NOT invoke finishing-a-development-branch; implement, test, commit, report.

**Goal:** A best-practice account settings surface: an editable Profile (display name, timezone, locale; read-only email + org/role memberships), Security (active sessions list, revoke a session, sign out everywhere), and Appearance (reduced-motion and density preferences).

**Architecture:** Go BFF gains profile fields on `Account` + `Get/UpdateProfile`, an auth-sensitive `SessionStore` extension (session records with ids + metadata, `ListByAccount`/`Revoke`/`RevokeAll`, with token `Resolve` unchanged so existing auth keeps working), and console endpoints (`GET/PATCH /console/account`, `GET /console/account/sessions`, `DELETE /console/account/sessions/{id}`, `DELETE /console/account/sessions`). The SPA gains an Account settings view. Appearance prefs are client-side (localStorage applied to `<html>` data attributes; honored by CSS).

**Tech Stack:** Go (`internal/saas`), React+Vite+TS, TanStack Query, Vitest + vitest-axe.

**Scope note:** B2d of the B2 split. Deferred to B3 / follow-up (each needs the auth work landing in B3): personal access tokens, notification preferences, 2FA/passkey enrollment, a light theme (a light variant conflicts with the additive-glow Fluorescence aesthetic and needs a dedicated design decision).

**Security note:** the `SessionStore` change is an authentication-critical path. It must preserve the existing `Resolve(token)` contract exactly (revoked/unknown tokens are indistinguishable), and revoking a session must immediately invalidate that session's token. This task carries the security-sensitive-path discipline (CLAUDE.md): extra care + the cross-account isolation test (account A can never list or revoke account B's sessions).

## Global Constraints

- **Punctuation (strict):** no em (U+2014) or en (U+2013) dashes anywhere. Only `.` `,` `;` `:`; ASCII `-`. Verify each commit.
- **Commits:** conventional + DCO (`git commit -s`). **Staging:** explicit paths only.
- **Account isolation:** every account endpoint reads the caller's account id from request context (`CallerFromContext`) only; account A can never read or mutate account B's profile or sessions. Tested.
- **Session security:** `Resolve` semantics unchanged; revoking a session invalidates its token; no token value or hash is ever logged or returned (only opaque session ids + non-secret metadata).
- **Go style:** `fmt.Errorf("...: %w", err)`; gofmt clean; BOTH golangci-lint invocations clean; production code not errcheck-excluded.
- **Responsive + accessible (spec 4.6):** the settings view is responsive; forms labelled; tabs/sections accessible; axe zero violations.
- **TypeScript strict** clean; SPA suite exits 0; `go test ./internal/saas/...` green.

## File Structure

- `internal/saas/model.go` (modify) - add `DisplayName`, `Timezone`, `Locale` to `Account`.
- `internal/saas/account.go` (modify) - `Profile(ctx, accountID)` and `UpdateProfile(ctx, accountID, ProfileUpdate)`.
- `internal/saas/session.go` (modify) - session records + `ListByAccount`/`Revoke`/`RevokeAll`; `Resolve` unchanged.
- `internal/saas/console/account.go` (create) - `AccountView`, the profile + sessions handlers.
- `internal/saas/console/console.go` (modify) - register the account routes.
- Go tests alongside.
- `web/app/src/api.ts` (modify), `web/app/src/data/account-settings.ts` (create), `web/app/src/views/Settings.tsx` (create), `web/app/src/appearance.ts` (create) + applied in `App`/`main`.
- `web/app/src/nav/routes.tsx` (modify) - point `/settings` at `Settings`.
- `web/packages/brand/src/base.css` (modify) - settings + appearance styles + reduced-motion/density honoring.
- Tests alongside.

---

### Task 1: Account profile (Go)

**Files:**
- Modify: `internal/saas/model.go`, `internal/saas/account.go`
- Test: `internal/saas/account_test.go`

**Interfaces:**
- `Account` gains `DisplayName string`, `Timezone string`, `Locale string`.
- `type ProfileUpdate struct { DisplayName, Timezone, Locale string }`.
- `func (s *AccountService) Profile(ctx, accountID string) (Account, []Membership, error)` (the account + its memberships).
- `func (s *AccountService) UpdateProfile(ctx, accountID string, u ProfileUpdate) (Account, error)` (loads the account, applies non-empty fields, `PutAccount`s it; `ErrNotFound` if absent).

- [ ] **Step 1: Write the failing test (append to `internal/saas/account_test.go`)**

```go
func TestUpdateProfile(t *testing.T) {
	svc, _, ownerID, _ := seedOrgWithOwnerAndMember(t)
	if _, err := svc.UpdateProfile(context.Background(), ownerID, ProfileUpdate{DisplayName: "Alice A", Timezone: "Europe/Berlin", Locale: "en-GB"}); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	acct, _, err := svc.Profile(context.Background(), ownerID)
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if acct.DisplayName != "Alice A" || acct.Timezone != "Europe/Berlin" || acct.Locale != "en-GB" {
		t.Fatalf("profile not updated: %+v", acct)
	}
}
```

(Reuse the `seedOrgWithOwnerAndMember` helper from the B2c tests.)

- [ ] **Step 2: Run it, confirm it fails**

Run: `go test ./internal/saas/ -run TestUpdateProfile`
Expected: FAIL (undefined `UpdateProfile`/`Profile`/`ProfileUpdate`).

- [ ] **Step 3: Implement the fields + methods**

Add the three fields to `Account` in `model.go`. Add `ProfileUpdate`, `Profile`, `UpdateProfile` to `account.go` (read via `store.GetAccount`, apply non-empty fields, `store.PutAccount`; `Profile` also returns memberships via the existing membership listing). gofmt clean.

- [ ] **Step 4: Run it, confirm pass; full package; gofmt**

Run: `go test ./internal/saas/ -run TestUpdateProfile` then `go test ./internal/saas/`
Run: `gofmt -l internal/saas/model.go internal/saas/account.go`
Expected: PASS, gofmt clean, existing tests hold.

- [ ] **Step 5: Commit**

```bash
git add internal/saas/model.go internal/saas/account.go internal/saas/account_test.go
git commit -s -m "feat(saas): account profile fields with Profile and UpdateProfile"
```

---

### Task 2: Session records + management (Go, auth-sensitive)

**Files:**
- Modify: `internal/saas/session.go`
- Test: `internal/saas/session_test.go`

Read `internal/saas/session.go` fully first. The current `SessionStore` keeps `byHash map[string]string` (token hash -> account id). You will add a parallel session-record map WITHOUT changing the `Resolve(token)` contract.

**Interfaces:**
- `type Session struct { ID, AccountID string; CreatedAt time.Time; Label string }` (Label is a non-secret hint, e.g. "browser session"; NO token or hash).
- `Issue` gains an id + record (keep the existing signature working: add an `IssueSession(accountID, token, label) string` that returns the session id, and keep `Issue` as a wrapper, OR extend `Issue` carefully without breaking callers; check all callers of `Issue` first).
- `func (s *SessionStore) ListByAccount(accountID string) []Session` (most-recent-first; never another account's).
- `func (s *SessionStore) Revoke(accountID, sessionID string) error` (removes the session AND its token hash so `Resolve` of that token now fails; `ErrNotFound` if the session is not that account's).
- `func (s *SessionStore) RevokeAll(accountID string)` (all the account's sessions).
- `Resolve(token)` unchanged in behavior.

- [ ] **Step 1: Write the failing test `internal/saas/session_test.go` (extend)**

```go
func TestSessionListAndRevoke(t *testing.T) {
	store := NewSessionStore()
	id1 := store.IssueSession("acctA", "tokA1", "browser")
	_ = store.IssueSession("acctA", "tokA2", "cli")
	store.IssueSession("acctB", "tokB1", "browser")

	a := store.ListByAccount("acctA")
	if len(a) != 2 {
		t.Fatalf("acctA sessions = %d, want 2", len(a))
	}
	// acctA never sees acctB.
	for _, s := range a {
		if s.AccountID != "acctA" {
			t.Fatalf("leaked session for %s", s.AccountID)
		}
	}
	// Revoking a session invalidates its token.
	if err := store.Revoke("acctA", id1); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := store.Resolve("tokA1"); err == nil {
		t.Fatalf("revoked token must not resolve")
	}
	// The other session still resolves.
	if _, err := store.Resolve("tokA2"); err != nil {
		t.Fatalf("tokA2 should still resolve: %v", err)
	}
	// acctA cannot revoke acctB's session.
	bsel := store.ListByAccount("acctB")
	if err := store.Revoke("acctA", bsel[0].ID); err == nil {
		t.Fatalf("cross-account revoke must fail")
	}
}
```

- [ ] **Step 2: Run it, confirm it fails**

Run: `go test ./internal/saas/ -run TestSessionListAndRevoke`
Expected: FAIL (undefined `IssueSession`/`ListByAccount`/`Revoke`).

- [ ] **Step 3: Implement the extension in `internal/saas/session.go`**

Keep `byHash`. Add `records map[string]Session` (session id -> record) and `tokenByID map[string]string` (session id -> token hash) so `Revoke` can delete the hash from `byHash`. `IssueSession` generates an id (deterministic-safe; the store may use a counter under the lock, NOT `Math/rand`/clock in a way that breaks tests), stores the record + the byHash entry + tokenByID. `Resolve` unchanged. `ListByAccount` filters records by accountID. `Revoke` checks the record belongs to the account (else `ErrNotFound`), deletes the record, its byHash entry, and tokenByID. `RevokeAll` loops. All under the mutex. Find and update every caller of the old `Issue` (e.g. the login flow `LoginManager`) to use `IssueSession` or keep `Issue` as a label-defaulting wrapper.

- [ ] **Step 4: Run it; full package; build; gofmt**

Run: `go test ./internal/saas/ -run TestSession` then `go test ./internal/saas/`
Run: `go build ./...` (catches broken `Issue` callers)
Run: `gofmt -l internal/saas/session.go`
Expected: PASS, build clean, gofmt clean.

- [ ] **Step 5: Commit**

```bash
git add internal/saas/session.go internal/saas/session_test.go
git commit -s -m "feat(saas): session records with per-account list and revoke"
```

---

### Task 3: Console account endpoints (Go)

**Files:**
- Create: `internal/saas/console/account.go`
- Modify: `internal/saas/console/console.go`
- Test: `internal/saas/console/account_test.go`

Read the console seam/handler patterns first.

**Interfaces:**
- `AccountView = { account_id, email, display_name, timezone, locale, memberships: []MemberView }`; `SessionView = { id, label, created_at, current: bool }`.
- Routes: `GET /console/account` (profile + memberships), `PATCH /console/account` (body `{display_name, timezone, locale}`), `GET /console/account/sessions`, `DELETE /console/account/sessions/{id}`, `DELETE /console/account/sessions`.
- Handlers read the caller account id from `CallerFromContext`; the sessions endpoints call the `SessionStore` (a new `Deps.Sessions` seam, or reuse the account service if it exposes it). Keep it simple: add a narrow `SessionLister` seam to `Deps` (`ListByAccount`, `Revoke`, `RevokeAll`) defaulting to a no-op/in-memory in tests.

- [ ] **Step 1: Write the failing test `internal/saas/console/account_test.go`**

A test that `GET /console/account` returns the caller's profile, `PATCH` updates it, and `GET /console/account/sessions` lists only the caller's sessions (cross-account isolation). Model fixtures on the existing console tests; inject a fake `SessionLister` + the account service.

- [ ] **Step 2: Run it, confirm it fails**

Run: `go test ./internal/saas/console/ -run TestAccount`
Expected: FAIL.

- [ ] **Step 3: Implement `account.go` + wire routes/deps in `console.go`**

Add `AccountView`/`SessionView`, the `SessionLister` seam + `Deps.Sessions` (nil-default to an in-memory fake), register the five routes, implement the handlers (account from `CallerFromContext`; PATCH via `AccountService.UpdateProfile`; sessions via the seam; mark the caller's current session id if available, else `current:false`). Audit profile changes and session revokes via `c.audit`. Add the new routes to the auth-gate table in `console_test.go`.

- [ ] **Step 4: Run tests; full package; both lint; gofmt**

Run: `go test ./internal/saas/console/` ; `go build ./...`
Run: `gofmt -l internal/saas/console/account.go internal/saas/console/console.go`
Run: `golangci-lint run --timeout=5m ./internal/saas/... && GOOS=linux golangci-lint run --timeout=5m ./internal/saas/...`
Expected: all green/clean.

- [ ] **Step 5: Commit**

```bash
git add internal/saas/console/account.go internal/saas/console/console.go internal/saas/console/account_test.go
git commit -s -m "feat(console): account profile + session management endpoints"
```

---

### Task 4: Settings view + appearance (frontend)

**Files:**
- Modify: `web/app/src/api.ts`, `web/app/src/nav/routes.tsx`, `web/app/src/main.tsx`
- Create: `web/app/src/data/account-settings.ts`, `web/app/src/views/Settings.tsx`, `web/app/src/appearance.ts`
- Test: `web/app/src/views/Settings.test.tsx`, `web/app/src/appearance.test.ts`

**Interfaces:**
- `AccountView`, `SessionView` types; `api.account()`, `api.updateAccount(patch)`, `api.sessions()`, `api.revokeSession(id)`, `api.revokeAllSessions()`.
- Hooks: `useAccount`, `useUpdateAccount`, `useSessions`, `useRevokeSession`, `useRevokeAllSessions`.
- `appearance.ts`: `getAppearance()/setAppearance({reducedMotion, density})` persisting to localStorage and applying `document.documentElement.dataset` (`reduceMotion`, `density`); `applyAppearanceOnLoad()` called from `main.tsx`.
- `Settings` view: a `Tabs`-based view (Profile, Security, Appearance). Profile: editable display name / timezone / locale form + read-only email + memberships (with role badges). Security: the sessions table (label, created, current) with Revoke per row + a Sign out everywhere button. Appearance: reduced-motion toggle + density select, applied immediately.

- [ ] **Step 1: Write the failing tests**

`Settings.test.tsx`: render `/settings`, assert the Profile shows the email and an editable display-name input; switch to Security and see a session row; switch to Appearance and toggle reduced motion (assert `document.documentElement.dataset.reduceMotion` becomes set). `appearance.test.ts`: `setAppearance({reducedMotion:true,density:'compact'})` then `getAppearance()` round-trips and the dataset is applied.

- [ ] **Step 2: Run them, confirm they fail**

Run: `pnpm -C web/app test src/views/Settings.test.tsx src/appearance.test.ts`
Expected: FAIL.

- [ ] **Step 3: Implement api.ts additions, `appearance.ts`, `data/account-settings.ts`, `Settings.tsx`, route `/settings`, and `applyAppearanceOnLoad()` in `main.tsx`.**

- [ ] **Step 4: Run tests, full suite, typecheck**

Run: `pnpm -C web/app test src/views/Settings.test.tsx src/appearance.test.ts && pnpm -C web/app test && pnpm -C web/app typecheck`
Expected: PASS, clean.

- [ ] **Step 5: Commit**

```bash
git add web/app/src/api.ts web/app/src/data/account-settings.ts web/app/src/views/Settings.tsx web/app/src/appearance.ts web/app/src/nav/routes.tsx web/app/src/main.tsx web/app/src/views/Settings.test.tsx web/app/src/appearance.test.ts
git commit -s -m "feat(console): account settings view (profile, security sessions, appearance)"
```

---

### Task 5: Styles, a11y, final verification

**Files:**
- Modify: `web/packages/brand/src/base.css`
- Create: `web/app/src/views/Settings.a11y.test.tsx`

- [ ] **Step 1: Append token-driven settings styles** (`.settings-section`, `.kv` reuse, a density rule honoring `html[data-density="compact"]` tightening table/row padding, and a `html[data-reduce-motion="true"] *` rule disabling transitions/animations - complementing the existing `prefers-reduced-motion` block). No raw hex. Mobile rule.

- [ ] **Step 2: Write the axe a11y test `Settings.a11y.test.tsx`** (render `/settings`, assert zero violations on the Profile and Security tabs; tab controls and form inputs labelled). Fix any real violation.

- [ ] **Step 3: Final verification**

Run: `pnpm -C web/app test` (exit 0) ; `pnpm -C web/app typecheck` (clean) ; `pnpm -C web/app build` (succeeds)
Run: `go test ./internal/saas/...` (green)
Run: `golangci-lint run --timeout=5m ./internal/saas/... && GOOS=linux golangci-lint run --timeout=5m ./internal/saas/...` (clean)
Run: `grep -rnP '\xe2\x80\x94|\xe2\x80\x93' web/app/src internal/saas/console/account.go internal/saas/session.go web/packages/brand/src/base.css` (empty)

- [ ] **Step 4: Commit**

```bash
git add web/packages/brand/src/base.css web/app/src/views/Settings.a11y.test.tsx
git commit -s -m "feat(console): settings styles and B2d accessibility checks"
```

---

## Self-Review

**Spec coverage (section 5.5):** editable profile (Task 1, 4: display name, timezone, locale; read-only email + memberships); security sessions (Task 2, 3, 4: list, revoke, sign-out-everywhere); appearance (Task 4: reduced-motion + density). Covered. Deferred (noted): personal access tokens, notifications, 2FA/passkeys, light theme (each needs the B3 auth work or a dedicated design decision).

**Security:** the session-store change preserves `Resolve`, invalidates revoked tokens, and is cross-account isolated (Task 2 test). Account endpoints read the caller from context only and are isolation-tested (Task 3). The session store is a security-sensitive path: full review + named-human review before merge per CLAUDE.md.

**Type consistency:** Go `AccountView`/`SessionView` JSON tags match the TS types (Task 4); `Profile`/`UpdateProfile` (Task 1) and the session methods (Task 2) back the endpoints (Task 3) consumed by the hooks (Task 4). No drift.
