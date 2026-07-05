# Hosted Live Fork Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `sandbox.fork()` against api.mitos.run produces a child that inherits the parent's current runtime state (a true live fork), is billed, respects the secrets gate, and reports a real fork time.

**Architecture:** The live-fork engine already works end to end from a `Sandbox` object with `spec.source.fromSandbox` (`internal/controller/sandboxfork_controller.go`, dispatched by `sandbox_v2_controller.go` behind `enforceForkBudget`). The Python SDKs ALREADY post to `POST /v1/sandboxes/<id>/fork` with `{"id","template","pause_source":true}`; the gateway currently maps that path to `sandbox.create` (an explicit documented stopgap at `internal/saas/gateway.go:169`), so the body's template becomes a cold pool claim. The fix is server-side: (1) gateway maps the path to a new op `sandbox.fork`, counts it as a create for quota, and emits `sandbox.forked` telemetry; (2) the control plane gains a `fork()` handler that org-verifies the source via `getOwned`, materializes the `fromSandbox` Sandbox, translates the controller's `Rejected/SecretInheritanceDenied` condition into the structured LLM-legible error, and returns the create-shaped payload with a real `fork_time_ms`; (3) fork children pods get the `mitos.run/claim` label so the usage scraper bills them and the lifetime-terminate path can reap them (today they carry only husk/fork/org labels and are silently excluded from metering); (4) the TypeScript direct/server-mode `Sandbox` gains the forker capability (it currently throws `fork_unsupported`).

**Tech Stack:** Go (gateway, control plane, controller; controller-runtime fake client tests), Python SDK (no changes needed), TypeScript SDK.

## Global Constraints

- Never use em (U+2014) or en (U+2013) dashes anywhere: code, comments, commit messages, PR text. Connectors limited to `.` `,` `;` `:`.
- Error wrapping `fmt.Errorf("context: %w", err)`; octal literals `0o644`.
- Conventional commits with DCO: every commit via `git commit -s`.
- TDD: failing test first, same commit.
- Secret VALUES never in logs or error messages.
- Every error path returns the `{error:{code,message,cause,remediation}}` envelope with actionable remediation (`internal/apierr`).
- Lint gate: BOTH `golangci-lint run --timeout=5m` AND `GOOS=linux golangci-lint run --timeout=5m`.
- Threat-model delta lands in the same PR (`docs/threat-model.md`).
- Working tree: `/Users/jannesstubbemann/repos/mitos-run/mitos/.claude/worktrees/hosted-offer-gaps` (main checkout).
- SDK response contract (Python raises KeyError otherwise): the fork response MUST contain `id`, `endpoint`, `token`, `phase`, `template_id`, `fork_time_ms`.

---

### Task 1: Gateway: op sandbox.fork, quota-as-create, telemetry

**Files:**
- Modify: `internal/saas/gateway.go` (`opFromPath` ~line 169; telemetry emit block ~lines 304-320)
- Modify: `internal/saas/quota/enforcer.go` (`isCreate` ~line 78) and `internal/saas/quota/gateway_adapter.go` (`Check` ~lines 49-58)
- Test: `internal/saas/gateway_livefork_test.go` (update), `internal/saas/quota/enforcer_test.go` (append)

**Interfaces:**
- Consumes: existing `opFromPath`, `requiredScopeFor` (fails closed to `ScopeSandboxes` for unknown ops: no scope-map change), `isLifecycle` (already whitelists `"sandbox.fork"`).
- Produces: op string `"sandbox.fork"` for `POST /v1/sandboxes/<id>/fork`; Task 2's `Forward` dispatch consumes it. Quota semantics: a fork counts as a create (footprint, size, aggregate caps).

- [ ] **Step 1: Update the failing tests first**

In `internal/saas/gateway_livefork_test.go`, change the existing assertion:

```go
func TestOpFromPathLiveForkMapsToFork(t *testing.T) {
	if got := opFromPath(http.MethodPost, "/v1/sandboxes/sb-123/fork"); got != "sandbox.fork" {
		t.Fatalf("POST /v1/sandboxes/{id}/fork op = %q, want sandbox.fork", got)
	}
	// The flat template-fork route keeps its create semantics (back compat).
	if got := opFromPath(http.MethodPost, "/v1/fork"); got != "sandbox.create" {
		t.Fatalf("POST /v1/fork op = %q, want sandbox.create", got)
	}
}
```

(Keep the other assertions in that test file; rename the old function.) Append to `internal/saas/quota/enforcer_test.go` a test that a `sandbox.fork` request is subject to the create-path caps, modeled exactly on the existing `sandbox.create` cap test in that file (same fixture, op string swapped): a fork request over the concurrent-sandbox or size cap is denied.

- [ ] **Step 2: Run to verify failure**

Run: `cd /Users/jannesstubbemann/repos/mitos-run/mitos/.claude/worktrees/hosted-offer-gaps && go test ./internal/saas/ ./internal/saas/quota/ -run 'Fork|fork' -v`
Expected: the renamed gateway test FAILS (op still `sandbox.create`); the quota test FAILS (fork not treated as create).

- [ ] **Step 3: Implement**

1. `internal/saas/gateway.go` `opFromPath`: replace the live-fork stopgap case (and its long comment) with:

```go
	// Live fork (issue #596): POST /v1/sandboxes/<id>/fork forks the RUNNING
	// source sandbox (DirectSandbox._fork_one). The control plane materializes
	// a Sandbox with spec.source.fromSandbox; this is the hosted live-fork op.
	// The flat POST /v1/fork below keeps its create-from-template semantics.
	case strings.HasPrefix(p, "sandboxes/") && strings.HasSuffix(p, "/fork") && method == http.MethodPost:
		return "sandbox.fork"
```

2. Telemetry: widen the emit block so a successful fork emits its own event:

```go
	if (op == "sandbox.create" || op == "sandbox.fork") && status >= 200 && status < 300 && g.tel.Enabled() {
		props := map[string]any{"success": true}
		if pool := resp.Header.Get("X-Mitos-Pool"); pool != "" {
			props["pool"] = pool
		}
		name := "sandbox.created"
		if op == "sandbox.fork" {
			name = "sandbox.forked"
		}
		g.tel.Emit(r.Context(), telemetry.Event{
			Name:       name,
			OrgID:      orgID,
			Properties: props,
		})
	}
```

Also add `sandbox.forked` to the documented event list comment in `internal/saas/telemetry/telemetry.go` line 3.

3. Quota: in `internal/saas/quota/enforcer.go`:

```go
// isCreate reports whether the op admits a NEW sandbox; a live fork admits one
// exactly like a create, so both count against footprint and size caps.
func (r Request) isCreate() bool { return r.Op == "sandbox.create" || r.Op == "sandbox.fork" }
```

In `internal/saas/quota/gateway_adapter.go`, widen the `op == "sandbox.create"` branch that attaches `NewSandbox` size to also match `"sandbox.fork"`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/saas/ ./internal/saas/quota/ -v`
Expected: PASS, including the pre-existing gateway tests (`TestGatewayForkMapsToSandboxCreate` for `/v1/fork` is unaffected).

- [ ] **Step 5: Commit**

```bash
git add internal/saas/gateway.go internal/saas/gateway_livefork_test.go internal/saas/telemetry/telemetry.go internal/saas/quota/
git commit -s -m "feat(gateway): sandbox.fork op for POST /v1/sandboxes/<id>/fork; forks count as creates for quota (#596)"
```

---

### Task 2: Control plane: the fork handler

**Files:**
- Modify: `internal/saas/controlplane/forward.go` (dispatch switch ~line 37; new `fork()` + `forkBody`; new poll)
- Test: `internal/saas/controlplane/forward_fork_test.go` (create)

**Interfaces:**
- Consumes: `getOwned(ctx, orgID, name)` (org-label-verified source lookup), `namespaceForOrg`, `tenant.OrgLabels`, `generateName()`, `errResp`/`jsonResp`/`withStatus` + `apierr`, `v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name, PauseSource}}`, `v1.SecretInheritanceMode` (`reissue`/`inherit`), the fake-client test harness in `controlplane_test.go` (`newFakeClient`, `flipToReadyWhenCreated`).
- Produces: op `"sandbox.fork"` handled; response payload `{id, endpoint, token, phase, template_id, fork_time_ms}`. Task 4 (TS SDK) consumes this contract.

- [ ] **Step 1: Discovery (read, decide, note in the test file header)**

Read `internal/controller/sandboxfork_controller.go` (`reconcileFromSandbox`, `reconcileHuskFork`) and `internal/controller/sandbox_v2_controller.go` to determine, for a `fromSandbox` Sandbox with replicas 1: (a) which status field signals readiness (top-level `Status.Phase` vs `Status.ReadyReplicas`/`Status.Children`), (b) where the child endpoint lands (top-level `Status.Endpoint` vs `Status.Children[0].Endpoint`), (c) the token Secret name (`<name>-sandbox-token` for the fork object vs per-child), (d) where startup latency is recorded (`SandboxChild.StartupLatencyMs`). Write the four answers as a comment block at the top of the new test file. The fake-client tests then flip EXACTLY those fields (the `flipToReadyWhenCreated` helper mimics the controller; extend it or add `flipForkToReadyWhenCreated` accordingly). If the controller does not populate a top-level endpoint for replicas 1, the fork poll must read the child fields; do NOT change the controller in this task.

- [ ] **Step 2: Write the failing tests**

Create `internal/saas/controlplane/forward_fork_test.go` with these cases (model fixture and helper usage on the existing create tests in `controlplane_test.go`; `sourceReady` below means a Sandbox seeded with org label, `Status.Phase = v1.SandboxReady`, and a `PoolRef` to pool `python`):

```go
// TestForkBuildsFromSandboxObject: POST op sandbox.fork with path
// /v1/sandboxes/src-1/fork and body {"id":"child-1","pause_source":true}
// against a Ready org-owned source src-1 creates a Sandbox named child-1 in
// the org namespace with Spec.Source.FromSandbox.Name == "src-1",
// FromSandbox.PauseSource == true, PoolRef nil, org label set, and
// SecretInheritance empty (reissue default).

// TestForkResponseContract: with the ready-flipper running, the response is
// 201 and the payload carries id, endpoint, token, phase, template_id (the
// SOURCE's pool name "python"), and fork_time_ms as a number.

// TestForkOfForeignSandboxIsNotFound: source exists but carries a different
// org label; expect 404 not_found (never 403: do not leak existence).

// TestForkOfMissingSandboxIsNotFound: no such source; 404 with remediation
// naming GET /v1/sandboxes.

// TestForkSecretInheritanceDeniedSurfacesLLMLegibly: source has
// Spec.Secrets non-empty; body omits secret_inheritance. The flipper stamps
// the Rejected/SecretInheritanceDenied condition (exactly what the fork
// controller does) instead of Ready. Expect a 403 envelope whose remediation
// contains `"secret_inheritance": "inherit"`.

// TestForkSecretInheritanceOptInPassesThrough: body sets
// {"secret_inheritance":"inherit"}; the built object has
// Spec.SecretInheritance == v1.SecretInherit.

// TestForkInvalidChildIDIsInvalidInput: body {"id":"Bad_ID!"} yields 400
// invalid_input (DNS-1123 validation) with remediation.
```

Write them as real Go tests: construct `K8sControlPlane` exactly as the sibling create tests do, call `Forward(ctx, saas.ForwardRequest{OrgID: "alpha", Op: "sandbox.fork", Method: "POST", Path: "/v1/sandboxes/src-1/fork", Body: ...})`, and assert on the fake client's stored objects plus the JSON payload.

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/saas/controlplane/ -run TestFork -v`
Expected: FAIL (unknown operation "sandbox.fork" from the default dispatch case).

- [ ] **Step 4: Implement**

In `internal/saas/controlplane/forward.go`:

1. Dispatch: add `case "sandbox.fork": return k.fork(ctx, req)`.
2. Body and source-id parsing (the shared `idFromPath` returns "fork" for this path; parse the segment before it):

```go
type forkBody struct {
	ID                string `json:"id,omitempty"`
	Template          string `json:"template,omitempty"` // sent by the SDK; informational, the SOURCE determines the state
	PauseSource       bool   `json:"pause_source,omitempty"`
	SecretInheritance string `json:"secret_inheritance,omitempty"`
}

// forkSourceID extracts <id> from /v1/sandboxes/<id>/fork.
func forkSourceID(path string) string {
	p := strings.TrimPrefix(strings.Trim(path, "/"), "v1/")
	p = strings.TrimPrefix(p, "sandboxes/")
	parts := strings.Split(p, "/")
	if len(parts) == 2 && parts[1] == "fork" && parts[0] != "" {
		return parts[0]
	}
	return ""
}
```

3. The handler (follow `create`'s error-envelope style exactly):

```go
func (k *K8sControlPlane) fork(ctx context.Context, req saas.ForwardRequest) (saas.ForwardResponse, error) {
	sourceID := forkSourceID(req.Path)
	if sourceID == "" {
		return errResp(apierr.Get(apierr.CodeInvalidInput).
			WithCause("the fork path does not name a source sandbox").
			WithRemediation("POST /v1/sandboxes/<id>/fork where <id> is a running sandbox from GET /v1/sandboxes.")), nil
	}
	var body forkBody
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return errResp(apierr.Get(apierr.CodeInvalidJSON).
				WithCause("the fork request body is not valid JSON")), nil
		}
	}
	if body.SecretInheritance != "" && body.SecretInheritance != string(v1.SecretReissue) && body.SecretInheritance != string(v1.SecretInherit) {
		return errResp(apierr.Get(apierr.CodeInvalidInput).
			WithCause(fmt.Sprintf("secret_inheritance %q is not a mode", body.SecretInheritance)).
			WithRemediation("Use \"reissue\" (default: the fork gets fresh credentials) or \"inherit\" (explicit opt-in: the fork duplicates the source's in-memory secrets).")), nil
	}

	source, ok := k.getOwned(ctx, req.OrgID, sourceID)
	if !ok {
		return errResp(apierr.Get(apierr.CodeNotFound).
			WithMessage(fmt.Sprintf("no such sandbox %q", sourceID)).
			WithCause("the source sandbox does not exist in this org").
			WithRemediation("List sandboxes with GET /v1/sandboxes and fork a running one.")), nil
	}

	name := body.ID
	if name == "" {
		name = generateName()
	} else if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return errResp(apierr.Get(apierr.CodeInvalidInput).
			WithCause(fmt.Sprintf("the requested fork id %q is not a valid name: %s", name, errs[0])).
			WithRemediation("Use lowercase alphanumerics and hyphens (RFC 1123), or omit id to have one generated.")), nil
	}

	ns := k.namespaceForOrg(req.OrgID)
	sb := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    tenant.OrgLabels(req.OrgID),
		},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{
				FromSandbox: &v1.FromSandboxSource{Name: source.Name, PauseSource: body.PauseSource},
			},
		},
	}
	if body.SecretInheritance != "" {
		sb.Spec.SecretInheritance = v1.SecretInheritanceMode(body.SecretInheritance)
	}

	started := time.Now()
	if err := k.c.Create(ctx, sb); err != nil {
		// mirror create()'s error triage verbatim (AlreadyExists conflict,
		// namespace missing, invalid, internal)
	}
	return k.pollForkReady(ctx, ns, name, req.OrgID, source, started)
}
```

4. `pollForkReady`: clone `pollReady`'s loop but resolve readiness, endpoint, and token per the Step 1 discovery. Terminal conditions: readiness signal reached (201 via a fork variant of `readyResponse`); a `Rejected` condition (read `meta.FindStatusCondition(sb.Status.Conditions, "Rejected")`) becomes:

```go
		if c := meta.FindStatusCondition(sb.Status.Conditions, "Rejected"); c != nil && c.Status == metav1.ConditionTrue {
			e := apierr.Get(apierr.CodeForbidden).
				WithMessage("fork rejected").
				WithCause(c.Message)
			if c.Reason == "SecretInheritanceDenied" {
				e = e.WithRemediation("The source sandbox holds secrets; re-request the fork with \"secret_inheritance\": \"inherit\" to explicitly permit duplicating them into the fork, or fork a sandbox without secrets.")
			} else {
				e = e.WithRemediation("Inspect the sandbox with GET /v1/sandboxes/<id> for the rejection reason.")
			}
			return errResp(withStatus(e, http.StatusForbidden)), nil
		}
```

`SandboxFailed` and deadline behave exactly as `pollReady`. The success payload:

```go
	templateID := ""
	if source.Spec.Source.PoolRef != nil {
		templateID = source.Spec.Source.PoolRef.Name
	}
	forkMs := float64(time.Since(started).Milliseconds())
	// prefer the controller-recorded child StartupLatencyMs when present (Step 1 discovery)
	payload := map[string]any{
		"id":           sb.Name,
		"endpoint":     endpoint,
		"token":        token,
		"phase":        string(v1.SandboxReady),
		"template_id":  templateID,
		"fork_time_ms": forkMs,
	}
	return jsonResp(http.StatusCreated, payload), nil
```

Set the `X-Mitos-Pool` response header to `templateID` when non-empty (the gateway telemetry reads it), matching however `create`'s response sets it (check `jsonResp`/`ForwardResponse.Header` usage in `create`; mirror it).

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/saas/controlplane/ -v`
Expected: PASS, all new TestFork* plus the untouched create/status/list/terminate suites.

- [ ] **Step 6: Commit**

```bash
git add internal/saas/controlplane/
git commit -s -m "feat(controlplane): sandbox.fork materializes a fromSandbox live fork with org-scoped source and LLM-legible secrets gate (#596)"
```

---

### Task 3: Fork children are billed and reaped: stamp the claim label

**Files:**
- Modify: `internal/controller/huskpod.go` (`buildForkChildPod` ~lines 990-1013)
- Test: `internal/controller/usage_scrape_test.go` (append; find the existing `HuskPodScrapeLister` test in the package and model on it)

**Interfaces:**
- Consumes: label constants `huskLabel`, `huskClaimLabel`, `huskForkLabel`; `HuskPodScrapeLister.ListHuskPods` (selects `husk=true` + HasLabels claim, filters Running + org label).
- Produces: fork children pods carry `mitos.run/claim: <fork Sandbox name>` in addition to `mitos.run/fork`. This makes them (a) visible to the billing scraper with `APIID` = the hosted fork id, and (b) reapable by the claim-label pod-deletion paths (`reconcileDelete` and the #688 `terminateLifetime` fix), so a fork hitting lifetime limits stops and stops billing like any claim.

- [ ] **Step 1: Write the failing test**

Append to the usage-scrape test file a case building a fake fork-child pod:

```go
func TestScrapeListerIncludesForkChildren(t *testing.T) {
	forkChild := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sb-child-1-0",
			Namespace: "mitos-org-acme",
			Labels: map[string]string{
				huskLabel:          "true",
				huskForkLabel:      "sb-child-1",
				huskClaimLabel:     "sb-child-1",
				tenant.OrgLabelKey: "acme",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.9"},
	}
	// build the lister exactly as the sibling test does, List, and assert one
	// usage.HuskPod with VMID "sb-child-1-0", APIID "sb-child-1", OrgID "acme".
}

func TestBuildForkChildPodCarriesBillingLabels(t *testing.T) {
	fork := &v1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sb-child-1", Namespace: "mitos-org-acme"}}
	pod := buildForkChildPod(fork, "sb-child-1-0", HuskPodOptions{}, testScheme(t)) // reuse however sibling tests obtain a scheme
	if pod.Labels[huskClaimLabel] != "sb-child-1" {
		t.Fatalf("fork child claim label = %q, want the fork name (billing scraper and terminate paths select on it)", pod.Labels[huskClaimLabel])
	}
	if pod.Labels[huskForkLabel] != "sb-child-1" || pod.Labels[huskLabel] != "true" {
		t.Fatalf("fork child labels = %v", pod.Labels)
	}
	if pod.Labels[tenant.OrgLabelKey] != "acme" {
		t.Fatalf("org label = %q, want acme (derived from the namespace)", pod.Labels[tenant.OrgLabelKey])
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/controller/ -run 'TestBuildForkChildPodCarriesBillingLabels|TestScrapeListerIncludesForkChildren' -v`
Expected: `TestBuildForkChildPodCarriesBillingLabels` FAILS (no claim label). The lister test passes by construction (it fabricates the label); keep it, it pins the contract.

- [ ] **Step 3: Implement**

In `buildForkChildPod`, after `pod.Labels[huskForkLabel] = fork.Name`, add:

```go
	// The claim label carries the hosted sandbox id for a claimed pod; a fork
	// child is claimed by its fork Sandbox from birth. The usage scraper
	// selects on this label (a fork child without it is silently unbilled)
	// and the claim-label pod-deletion paths (release, lifetime terminate)
	// reap by it.
	pod.Labels[huskClaimLabel] = fork.Name
```

- [ ] **Step 4: Check for regressions in claim-label consumers**

Run: `go test ./internal/controller/ -timeout 20m` (plain unit run first; then the envtest suite: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -timeout 20m`).
Expected: PASS. Pay attention to pool-reconciler tests (fork children carry no `mitos.run/pool` label, so warm-pool accounting must be unaffected) and any fork envtest asserting exact label sets: update those assertions to include the new label if they enumerate labels exhaustively.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/huskpod.go internal/controller/usage_scrape_test.go
git commit -s -m "fix(controller): fork children carry the claim label so they are billed and lifetime-reaped (#596)"
```

---

### Task 4: TypeScript SDK: direct/server-mode fork capability

**Files:**
- Modify: `sdk/typescript/src/server.ts` (the `Sandbox` construction ~lines 223-230; wire shapes ~lines 109-122)
- Test: `sdk/typescript/test/` (find the server-mode conformance test file driving the mock server; append)

**Interfaces:**
- Consumes: the Task 2 response contract `{id, endpoint, token, phase, template_id, fork_time_ms}`; the existing `Forker` type and `ForkOptions {pauseSource?}` in `sandbox.ts`; the mock-server test harness the 31 existing conformance tests use.
- Produces: `sandbox.fork(n, {pauseSource})` works in TS server/direct mode by POSTing `/v1/sandboxes/<id>/fork` once per child.

- [ ] **Step 1: Write the failing test**

Append to the server-mode conformance tests (mirror the style of the existing create/exec tests against the mock server):

```typescript
test("fork() posts to /v1/sandboxes/<id>/fork and returns live-fork children", async () => {
  // mock server: register POST /v1/sandboxes/sb-parent/fork returning
  // { id: "sb-kid", endpoint: "127.0.0.1:9999", token: "t", phase: "Ready",
  //   template_id: "python", fork_time_ms: 27.0 }
  const parent = /* obtain a Sandbox from the mock create flow as sibling tests do */;
  const kids = await parent.fork(1, { pauseSource: true });
  expect(kids).toHaveLength(1);
  expect(kids[0].id).toBe("sb-kid");
  // the mock recorded exactly one POST to /v1/sandboxes/sb-parent/fork with
  // body containing pause_source: true
});
```

- [ ] **Step 2: Run to verify failure**

Run: `cd sdk/typescript && npm test -- --run fork` (use the package's actual test invocation from its package.json scripts)
Expected: FAIL with the `fork_unsupported` AgentRunError.

- [ ] **Step 3: Implement**

In `server.ts`, add a forker and pass it where the `Sandbox` is constructed (currently only `terminator` is passed):

```typescript
  private makeForker(sourceId: string): Forker {
    return async (n = 1, opts?: ForkOptions): Promise<Sandbox[]> => {
      const kids: Sandbox[] = [];
      for (let i = 0; i < n; i++) {
        const wire = await this.http.post<forkWire>(`/v1/sandboxes/${sourceId}/fork`, {
          pause_source: opts?.pauseSource ?? true,
        });
        kids.push(this.sandboxFromWire(wire));
      }
      return kids;
    };
  }
```

Adapt names to the file's actual helpers: it already has a `forkWire` decode and a wire-to-Sandbox construction for the template-fork path; reuse those, adding the forker argument to the `Sandbox` constructor call so the returned children can themselves fork.

- [ ] **Step 4: Run the SDK suite**

Run: `cd sdk/typescript && npm run build && npm test && npm run typecheck` (exact script names from package.json)
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add sdk/typescript/
git commit -s -m "feat(sdk-ts): server-mode fork() drives the hosted live-fork endpoint (#596)"
```

---

### Task 5: Docs, threat model, parity follow-up, lint

**Files:**
- Modify: `docs/threat-model.md`, the hosted API docs page documenting `/v1` routes (find: `grep -rln "v1/fork\|/v1/sandboxes" docs/ | grep -v superpowers | grep -v threat`), `sdk/python/README.md` and `sdk/typescript/README.md` fork sections
- Create: a GitHub issue for Go/Ruby/Rust/Java fork parity (do NOT implement)

**Interfaces:**
- Consumes: everything above.
- Produces: honest user-facing docs and the recorded security delta.

- [ ] **Step 1: Threat model delta**

In `docs/threat-model.md`, add to the hosted gateway surface section: the new authenticated `sandbox.fork` op; source resolution is org-label-verified (`getOwned`), a foreign or missing source is an indistinguishable 404; `secret_inheritance: inherit` is an explicit opt-in carried over the authenticated wire and audited by the controller's `SecretInheritance/ExplicitOptIn` condition; fork children are labeled, billed, and lifetime-reaped like claims; fork fan-out is budget-gated (`enforceForkBudget`) and quota-counted as creates.

- [ ] **Step 2: API and SDK docs**

Document `POST /v1/sandboxes/<id>/fork` (body `{id?, pause_source?, secret_inheritance?}`, response contract, the node-locality constraint: children run on the source's node by construction, so fan-out is bounded by that node's capacity) in the hosted API docs page. Update both SDK READMEs' fork examples to state the hosted semantic: the child inherits the parent's current memory and filesystem state. No internal issue refs in user-facing copy; check all edits for em/en dashes.

- [ ] **Step 3: File the parity follow-up issue**

```bash
gh issue create --title "SDK parity: live fork() for Go, Ruby, Rust, Java against POST /v1/sandboxes/<id>/fork" --body "Python and TypeScript drive the hosted live-fork endpoint (this PR). The other four SDKs still lack a direct-mode forker. Contract: POST /v1/sandboxes/<id>/fork with {id?, pause_source?, secret_inheritance?} returning {id, endpoint, token, phase, template_id, fork_time_ms}. Relates to the SDK parity goal.

Signed-off intent: docs-only issue, no dashes."
```

(Trim the body's last line; it is a reminder to keep the issue text dash-clean, not literal content. Write the issue body without em/en dashes.)

- [ ] **Step 4: Full verification**

Run: `go build ./... && go test ./internal/saas/... && golangci-lint run --timeout=5m && GOOS=linux golangci-lint run --timeout=5m`
Then the controller suites if not already run in Task 3.
Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add docs/ sdk/python/README.md sdk/typescript/README.md
git commit -s -m "docs: hosted live fork surface, threat model delta, SDK fork semantics (#596)"
```
