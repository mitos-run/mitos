# Hosted journey slice 4: email confirmation and anti-abuse (Workstream B)

> For agentic workers: execute with superpowers:subagent-driven-development, one task at a time, task-reviewed, then a slice final review. Steps are checkbox-tracked.

**Goal:** Make self-serve signup abuse-resistant so the $5 signup credit is grantable at most once per real person: canonical-email identity (Gmail dot/plus/googlemail folding), a disposable-domain blocklist, a per-IP velocity cap, and an optional Friendly Captcha hook. Defense in depth on top of the Slice A allowlist gate.

**Architecture:** Extend the `internal/saas/onboarding` funnel. Add a pure `canonicalEmail` that folds an address to a stable identity; the onboarding signup stores the canonical form as the account identity (so folded variants collapse to one account and one credit) while delivering the verify email to the ORIGINAL typed address. Add a disposable-domain check, a per-IP velocity cap, and a Friendly Captcha verification, all at the signup HTTP boundary, each failing safe and each returning the SAME uniform 202 so a prober learns nothing. One console artifact; every abuse control is off/pass-through when unconfigured so self-host is unaffected.

**Tech stack:** Go BFF (`internal/saas/onboarding`), the account store, an in-tree JSON blocklist, an in-memory + optional durable velocity store.

## Global Constraints (every task inherits these)

- No em or en dashes anywhere (code, comments, JSON, copy, commit messages). ASCII hyphen only.
- The email address (PII) and the verify token are NEVER logged; funnel events key on a hash. Captcha/secret keys are never logged.
- No enumeration: signup ALWAYS returns a uniform 202 whether the email is new, existing, disposable, rate-limited, or captcha-failed. The only non-202 is a malformed request (400) or a server fault (500). A rejection must not tell a prober which rule fired.
- Money is integer cents; the signup credit is grantable at most once per canonical identity.
- Self-host cleanliness: every abuse control is pass-through/disabled when unconfigured (no blocklist file, no velocity cap env, no captcha keys) so community/self-host signup is unchanged.
- Go: `fmt.Errorf("ctx: %w", err)`; gofmt + both golangci-lint invocations clean. TDD failing test first, same commit. DCO `git commit -s`. Conventional commits. Stage explicit paths only.

## What already exists (do not rebuild)

- `normalizeEmail(raw) (string, bool)` in `internal/saas/onboarding/http.go`: lowercases + parses the address (no folding). B1 layers folding on top; the delivery address keeps the original.
- `onboarding.Service.SignUp(ctx, email, useCase)`: in ModeOpen it stores a `PendingSignup{Email,...}` and emails a verify token; the account is created later at `Verify` via `accounts.SignUp(pending.Email)`. Account dedup is exact-email (`store.GetAccountByEmail`).
- The signup HTTP handler in `internal/saas/onboarding/http.go` (the `signup` method) is where request-level abuse checks belong (it has the `*http.Request`, so the client IP and captcha solution are available there).
- The Slice A allowlist gate already limits who provisions; B is defense in depth for a wider opening.

---

### Task B1a: pure canonicalEmail folding function

**Files:** Create `internal/saas/onboarding/canonical.go` + `internal/saas/onboarding/canonical_test.go`.

**Produces:** `func canonicalEmail(addr string) (string, bool)`: parse + lowercase (reuse/mirror `normalizeEmail`); drop a `+tag` suffix from the local part (provider-safe, all providers); for `gmail.com` and `googlemail.com` ALSO remove all `.` from the local part and fold the domain to `gmail.com`. Returns `("", false)` for a malformed address. Deterministic and total.

**Acceptance:** table tests: `U.s.e.r+x@Gmail.com` -> `user@gmail.com`; `user@googlemail.com` -> `user@gmail.com`; `a.b@gmail.com` == `ab@gmail.com`; a non-Gmail `First.Last+tag@Outlook.com` -> `first.last@outlook.com` (dots preserved, tag dropped, lowercased); malformed -> `("", false)`. No dashes.

### Task B1b: signup stores canonical identity, delivers to the original

**Files:** Modify `internal/saas/onboarding/service.go` (SignUp) + `internal/saas/onboarding/http.go` (signup handler) + their tests.

**Produces:** the onboarding signup canonicalizes the email; the PendingSignup identity and the eventual account use the canonical form (so folded variants collapse to one account, hence one signup credit), while the verification email is delivered to the ORIGINAL typed address. The `Verify` gate and the account dedup then operate on canonical, so `u.ser+x@gmail` and `user@gmail` are one identity. Keep the uniform 202.

**Acceptance:** signing up `U.ser+x@Gmail.com` then `user@gmail.com` yields ONE account/one pending identity (the second is the existing-identity path, still 202, no second provisioning later); the verify email is sent to the original address form; the canonical form is what `IsAllowed` and the account store see. Existing non-Gmail behavior unchanged. Tests assert the collapse and the original-address delivery.

### Task B2: disposable-domain blocklist

**Files:** Create `internal/saas/onboarding/disposable.go` + a bounded in-tree `internal/saas/onboarding/disposable_domains.json` + test.

**Produces:** a loader for the JSON blocklist (bounded, staff can extend) and a check at the signup handler: a disposable (or explicitly blocked) domain returns the SAME uniform 202 with NO pending record created and NO email sent (no enumeration). A staff allow path (an env allowlist of domains that overrides the blocklist). Absent file -> the check is a no-op (self-host).

**Acceptance:** a known disposable domain signup returns 202 but creates no pending record and sends no email; a normal domain is unaffected; the staff-allow env overrides a blocked domain; missing file disables the check. Tests cover each.

### Task B3: per-IP signup velocity cap

**Files:** Create `internal/saas/onboarding/velocity.go` + test; wire into the signup handler.

**Produces:** a per-IP sliding-window counter (default 10/hour, env `MITOS_CONSOLE_SIGNUP_IP_LIMIT` and window tunable) using the existing rate-limit facility if one exists, else a small in-memory store. Over the cap -> the uniform 202 with no provisioning. Fail closed on the cap (an over-limit IP is refused), but disabled when the limit env is unset/zero (self-host). The client IP is read from the trusted proxy header the rest of the app uses (confirm which), never spoofable from an arbitrary header.

**Acceptance:** the Nth+1 signup from one IP within the window returns 202 with no pending record; a different IP is unaffected; unset limit disables the cap; the IP is never logged in full if the app treats it as PII (match existing practice). Tests cover the cap, the reset, and the disabled case.

### Task B4: Friendly Captcha verification hook (optional, pass-through)

**Files:** Create `internal/saas/onboarding/captcha.go` + test; wire into the signup handler.

**Produces:** a `CaptchaVerifier` seam with a Friendly Captcha implementation (verify the solution server-side against the Friendly Captcha API, EU-sovereign) and a no-op pass-through used when the site key/secret are absent. A failed verification -> the uniform 202 with no provisioning. Keys from env; never logged. Tested against an httptest fake (asserts the request shape, no secret in errors).

**Acceptance:** with keys set, a bad/missing solution yields 202-no-provision and a good solution proceeds; with keys absent the verifier passes through (self-host); the secret never appears in a log or error. Tests use an httptest fake.

---

## Sequencing

B1a (pure fold) first; B1b (wire identity) depends on B1a and is the core anti-farming guarantee. B2, B3, B4 are independent request-boundary layers (any order) that each fail safe and preserve the uniform 202. Then a slice-B final whole-branch review focused on the no-enumeration guarantee and the once-per-canonical-identity credit. The website/deploy enablement (Slice A note) and open-signup flip stay deferred.
