# Computer-use headless Chromium + CDP template Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a first-party, CI-verified template that boots headless Chromium with a CDP endpoint inside a Mitos microVM, snapshotted live so a fork yields warm browsers, reachable over the existing authenticated expose proxy, with a runnable example and recipe.

**Architecture:** A debian-slim + Chromium OCI image whose launcher starts headless Chromium on guest loopback with `--remote-debugging-port=9222`. The serving-workload primitive (#460) snapshots it already listening; a fork restores N warm browsers. CDP (HTTP + WebSocket) rides the existing expose reverse proxy. The standalone `sandbox-server` template endpoint is extended to accept the `workload` and `resources` the engine already supports, so the warm-fork model runs on the no-Kubernetes path and is end-to-end verifiable on a single KVM host.

**Tech Stack:** Go (cmd/sandbox-server, internal/firecracker), Python SDK (sdk/python/mitos), Docker (image build), Firecracker microVM, Playwright (example/verification), GHCR.

## Global Constraints

- Punctuation: never use em (U+2014) or en (U+2013) dashes anywhere (source, comments, docs, YAML, commit messages, PR text). Use `.` `,` `;` `:` and ASCII hyphen-minus only.
- Go: error wrapping `fmt.Errorf("context: %w", err)`; octal as `0o644`; gofmt + golangci-lint clean is a merge gate; run BOTH `golangci-lint run --timeout=5m` and `GOOS=linux golangci-lint run --timeout=5m`.
- DCO: every commit carries `Signed-off-by` (`git commit -s`).
- Conventional commits: feat, fix, docs, ci, chore, refactor, test. Branch already `feat/computer-use-cdp-template`.
- Secrets: never log secret values; errors carry actionable remediation.
- TDD: failing test first, in the same commit as the behavior change.
- Stage explicit paths only; never `git add -A`.
- No unverified claims: every number/behavior the docs state must be reproducible (box1 run or a test).
- Box access: `ssh -F /Users/jannesstubbemann/repos/mitos-run/mitos/.superpowers/ssh_config box1` (firecracker host) or `box2` (docker + KVM host). Run box commands from the ORIGINAL repo root so the ssh_config path resolves. Always tear down box temp dirs after (shared runners).
- GHCR: new package images push private; making public is a one-time manual UI toggle.

---

### Task 1: Standalone sandbox-server accepts `workload` + `resources`

**Files:**
- Modify: `cmd/sandbox-server/main.go` (the `createTemplateReq` struct near line 414 and `handleCreateTemplate` near line 476)
- Test: `cmd/sandbox-server/main_test.go`

**Interfaces:**
- Consumes: `firecracker.WorkloadSpec{Command []string; Env map[string]string; Ready *firecracker.WorkloadHTTPReady}`, `firecracker.WorkloadHTTPReady{Port uint32; Path string; Expect uint32; TimeoutSeconds uint32}`, `firecracker.VMResources{VcpuCount int32; MemSizeMib int64}` (already defined in `internal/firecracker`).
- Produces: two pure mapping helpers other code and tests use:
  - `func workloadFromReq(w *workloadReq) *firecracker.WorkloadSpec`
  - `func vmResFromReq(r *resourcesReq) *firecracker.VMResources`
  and the extended JSON request types `workloadReq`, `workloadReadyReq`, `resourcesReq`.

- [ ] **Step 1: Write the failing test**

Add to `cmd/sandbox-server/main_test.go`:

```go
func TestWorkloadFromReq(t *testing.T) {
	if got := workloadFromReq(nil); got != nil {
		t.Fatalf("nil req should map to nil, got %v", got)
	}
	w := &workloadReq{
		Command: []string{"/usr/local/bin/start-chromium.sh"},
		Env:     map[string]string{"FOO": "bar"},
		Ready:   &workloadReadyReq{Port: 9222, Path: "/json/version", Expect: 200, TimeoutSeconds: 90},
	}
	got := workloadFromReq(w)
	if got == nil || len(got.Command) != 1 || got.Command[0] != "/usr/local/bin/start-chromium.sh" {
		t.Fatalf("command not mapped: %+v", got)
	}
	if got.Env["FOO"] != "bar" {
		t.Fatalf("env not mapped: %+v", got.Env)
	}
	if got.Ready == nil || got.Ready.Port != 9222 || got.Ready.Path != "/json/version" || got.Ready.Expect != 200 || got.Ready.TimeoutSeconds != 90 {
		t.Fatalf("ready not mapped: %+v", got.Ready)
	}
}

func TestVMResFromReq(t *testing.T) {
	if got := vmResFromReq(nil); got != nil {
		t.Fatalf("nil req should map to nil, got %v", got)
	}
	got := vmResFromReq(&resourcesReq{VcpuCount: 2, MemSizeMib: 1024})
	if got == nil || got.VcpuCount != 2 || got.MemSizeMib != 1024 {
		t.Fatalf("resources not mapped: %+v", got)
	}
}

func TestCreateTemplateReqDecodesWorkloadAndResources(t *testing.T) {
	body := `{"id":"chrome","workload":{"command":["/usr/local/bin/start-chromium.sh"],` +
		`"ready":{"port":9222,"path":"/json/version","expect":200,"timeout_seconds":90}},` +
		`"resources":{"vcpu_count":2,"mem_size_mib":1024}}`
	var req createTemplateReq
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Workload == nil || len(req.Workload.Command) != 1 {
		t.Fatalf("workload not decoded: %+v", req.Workload)
	}
	if req.Resources == nil || req.Resources.MemSizeMib != 1024 {
		t.Fatalf("resources not decoded: %+v", req.Resources)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd cmd/sandbox-server && go test ./... -run 'TestWorkloadFromReq|TestVMResFromReq|TestCreateTemplateReqDecodes' -v`
Expected: FAIL to compile (`workloadReq`, `workloadFromReq`, etc. undefined).

- [ ] **Step 3: Add the request types and mapping helpers**

In `cmd/sandbox-server/main.go`, replace the `createTemplateReq` struct with:

```go
type workloadReadyReq struct {
	Port           uint32 `json:"port"`
	Path           string `json:"path,omitempty"`
	Expect         uint32 `json:"expect,omitempty"`
	TimeoutSeconds uint32 `json:"timeout_seconds,omitempty"`
}

type workloadReq struct {
	Command []string          `json:"command,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Ready   *workloadReadyReq `json:"ready,omitempty"`
}

type resourcesReq struct {
	VcpuCount  int32 `json:"vcpu_count,omitempty"`
	MemSizeMib int64 `json:"mem_size_mib,omitempty"`
}

type createTemplateReq struct {
	ID           string         `json:"id"`
	InitWaitSecs int            `json:"init_wait_seconds"`
	Network      *networkConfig `json:"network,omitempty"`
	// Workload starts a long-running process during template build so the
	// snapshot captures it already serving and a fork wakes warm (issue #460).
	// The engine already supports this; the cluster path wires it via forkd.
	Workload *workloadReq `json:"workload,omitempty"`
	// Resources sizes the build VM. Omitted leaves the engine default (512 MiB,
	// 1 vCPU); a Chromium template needs more.
	Resources *resourcesReq `json:"resources,omitempty"`
}

// workloadFromReq maps the JSON workload to the engine's firecracker.WorkloadSpec.
// nil maps to nil so the engine keeps its no-workload default.
func workloadFromReq(w *workloadReq) *firecracker.WorkloadSpec {
	if w == nil || len(w.Command) == 0 {
		return nil
	}
	spec := &firecracker.WorkloadSpec{Command: w.Command, Env: w.Env}
	if w.Ready != nil {
		spec.Ready = &firecracker.WorkloadHTTPReady{
			Port:           w.Ready.Port,
			Path:           w.Ready.Path,
			Expect:         w.Ready.Expect,
			TimeoutSeconds: w.Ready.TimeoutSeconds,
		}
	}
	return spec
}

// vmResFromReq maps the JSON resources to the engine's firecracker.VMResources.
// nil maps to nil so the engine keeps its default sizing.
func vmResFromReq(r *resourcesReq) *firecracker.VMResources {
	if r == nil || (r.VcpuCount == 0 && r.MemSizeMib == 0) {
		return nil
	}
	return &firecracker.VMResources{VcpuCount: r.VcpuCount, MemSizeMib: r.MemSizeMib}
}
```

Confirm `internal/firecracker` is imported in `main.go` (it is, for `JailerConfig`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd cmd/sandbox-server && go test ./... -run 'TestWorkloadFromReq|TestVMResFromReq|TestCreateTemplateReqDecodes' -v`
Expected: PASS.

- [ ] **Step 5: Wire the helpers into the handler**

In `handleCreateTemplate`, replace the real-mode CreateTemplate call:

```go
	} else if s.engine != nil {
		if err := s.engine.CreateTemplate(req.ID, s.rootfsPath, nil, nil, workloadFromReq(req.Workload), vmResFromReq(req.Resources)); err != nil {
			s.releaseIdempotent(idemKey)
			errResp(w, fmt.Sprintf("create template: %v", err), 500)
			return
		}
	}
```

- [ ] **Step 6: Build, vet, lint**

Run: `go build ./cmd/sandbox-server/ && go vet ./cmd/sandbox-server/ && golangci-lint run cmd/sandbox-server/... --timeout=5m && GOOS=linux golangci-lint run cmd/sandbox-server/... --timeout=5m`
Expected: clean.

- [ ] **Step 7: Commit**

```bash
git add cmd/sandbox-server/main.go cmd/sandbox-server/main_test.go
git commit -s -m "feat(sandbox-server): accept workload and resources on POST /v1/templates (#314)

The fork engine already supports a serving workload (#460) and per-template VM
sizing; the standalone handler dropped them. Map them from JSON and pass them
through so the warm-fork model and a memory override work on the no-k8s path."
```

---

### Task 2: Python SDK passes `workload` + `resources` on template create

**Files:**
- Modify: `sdk/python/mitos/direct.py` (`create_template` near line 563, `ensure_template`, the flat `create` and `DirectSandbox.create`)
- Test: `sdk/python/tests/test_direct.py` (or the existing direct-mode test module; create `test_template_workload.py` if cleaner)

**Interfaces:**
- Consumes: nothing new.
- Produces: `create_template(..., workload: Optional[dict] = None, resources: Optional[dict] = None)` adds `workload` / `resources` keys to the POST body when set; same optional args threaded through `ensure_template`, `create`, and `DirectSandbox.create`.

- [ ] **Step 1: Write the failing test**

```python
def test_create_template_includes_workload_and_resources(monkeypatch):
    captured = {}

    class _FakeResp:
        status_code = 200
        def json(self):
            return {"id": "chrome", "ready": True, "creation_time_ms": 1.0}
        @property
        def text(self):
            return ""

    class _FakeHTTP:
        def post(self, url, json=None, headers=None):
            captured["body"] = json
            return _FakeResp()

    from mitos.direct import SandboxServer
    srv = SandboxServer(server_url="http://x", api_key=None)
    srv._http = _FakeHTTP()
    srv.create_template(
        "chrome",
        workload={"command": ["/usr/local/bin/start-chromium.sh"],
                  "ready": {"port": 9222, "path": "/json/version", "expect": 200}},
        resources={"vcpu_count": 2, "mem_size_mib": 1024},
    )
    assert captured["body"]["workload"]["command"] == ["/usr/local/bin/start-chromium.sh"]
    assert captured["body"]["resources"]["mem_size_mib"] == 1024
```

Adjust `SandboxServer` construction to match its real constructor (check `sdk/python/mitos/direct.py`; if it needs different args, use them; the test only needs `_http` swapped and `create_template` called).

- [ ] **Step 2: Run test to verify it fails**

Run: `cd sdk/python && PYTHONPATH=. python3 -m pytest tests/ -k create_template_includes -q`
Expected: FAIL (`workload` / `resources` not in body).

- [ ] **Step 3: Thread the arguments through `create_template`**

In `sdk/python/mitos/direct.py`, extend `create_template`:

```python
    def create_template(
        self,
        id: str,
        init_wait_seconds: int = 5,
        network: Optional[Network] = None,
        idempotency_key: Optional[str] = None,
        workload: Optional[dict] = None,
        resources: Optional[dict] = None,
    ) -> dict:
        body: dict = {"id": id, "init_wait_seconds": init_wait_seconds}
        if network is not None:
            body["network"] = network.to_dict()
        if workload is not None:
            body["workload"] = workload
        if resources is not None:
            body["resources"] = resources
        resp = self._http.post(
            f"{self.url}/v1/templates",
            json=body,
            headers=self._creating_headers(idempotency_key),
        )
        raise_for_status(resp, token=self._api_key)
        return resp.json()
```

Then add the same `workload`/`resources` optional params to `ensure_template` (pass them to `create_template`), to the flat `create(...)`, and to `DirectSandbox.create(...)` (pass to `ensure_template`). Keep them keyword-only-style optional with `None` defaults so existing callers are unaffected.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd sdk/python && PYTHONPATH=. python3 -m pytest tests/ -k 'create_template or template' -q`
Expected: PASS. Then run the full direct-mode suite: `PYTHONPATH=. python3 -m pytest tests/test_direct.py -q` (or the relevant module) and confirm no regressions.

- [ ] **Step 5: Commit**

```bash
git add sdk/python/mitos/direct.py sdk/python/tests/
git commit -s -m "feat(sdk-python): pass workload and resources on template create (#314)"
```

---

### Task 3: Chromium template image and launcher

**Files:**
- Create: `images/computer-use/Dockerfile`
- Create: `images/computer-use/start-chromium.sh`
- Create: `images/computer-use/README.md`

**Interfaces:**
- Produces: an OCI image whose `/usr/local/bin/start-chromium.sh` starts headless Chromium listening on `127.0.0.1:9222` with a CDP HTTP endpoint at `/json/version`. The image ships `setsid` and `/bin/sh` (debian-slim) for the serving-workload path.

- [ ] **Step 1: Write the launcher**

`images/computer-use/start-chromium.sh`:

```sh
#!/bin/sh
# Start headless Chromium with a CDP endpoint on guest loopback. Run as the
# serving workload so the template snapshot captures it already listening and a
# fork wakes warm. The microVM is the isolation boundary, so Chromium's own
# sandbox is disabled (--no-sandbox); see docs/threat-model.md.
set -eu
mkdir -p /data/chrome
exec chromium \
  --headless=new \
  --no-sandbox \
  --disable-dev-shm-usage \
  --disable-gpu \
  --remote-debugging-address=127.0.0.1 \
  --remote-debugging-port=9222 \
  --user-data-dir=/data/chrome \
  about:blank
```

- [ ] **Step 2: Write the Dockerfile**

`images/computer-use/Dockerfile`:

```dockerfile
# Headless Chromium + CDP template for Mitos browser-automation sandboxes (#314).
# The microVM is the isolation boundary; Chromium runs with --no-sandbox.
FROM debian:stable-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      chromium \
      fonts-liberation \
      ca-certificates \
 && rm -rf /var/lib/apt/lists/*

COPY start-chromium.sh /usr/local/bin/start-chromium.sh
RUN chmod +x /usr/local/bin/start-chromium.sh

# CDP endpoint. Reach it over the Mitos expose proxy, never publicly.
EXPOSE 9222
```

- [ ] **Step 3: Build the image locally and smoke-test CDP**

Run (on a docker host; box2 has docker):

```bash
docker build -t mitos-computer-use:dev images/computer-use/
# Smoke: start the launcher in the container, confirm CDP answers.
docker run -d --name cu-smoke --entrypoint /usr/local/bin/start-chromium.sh mitos-computer-use:dev
sleep 5
docker exec cu-smoke sh -c 'apt-get install -y curl >/dev/null 2>&1 || true; curl -fsS http://127.0.0.1:9222/json/version'
docker rm -f cu-smoke
```

Expected: `/json/version` returns a JSON document containing `"webSocketDebuggerUrl"`. If Chromium needs more than the container default memory, note it (the pool sets 1 GiB).

- [ ] **Step 4: Write the image README**

`images/computer-use/README.md`: what the image is, the CDP port, the `--no-sandbox` rationale (microVM is the boundary), and that it is meant to run as a Mitos serving workload behind the expose proxy. Link `docs/recipes/browser-automation.md`.

- [ ] **Step 5: Commit**

```bash
git add images/computer-use/Dockerfile images/computer-use/start-chromium.sh images/computer-use/README.md
git commit -s -m "feat(images): headless Chromium + CDP computer-use template image (#314)"
```

---

### Task 4: Spike on a KVM host: warm Chromium snapshot, fork survival, and CDP exposure path

This task resolves the spec's open questions (proxy-only host normalization versus an in-image CDP relay, and whether Chromium survives snapshot/restore) on real hardware before any user-facing doc claims a behavior. It produces a recorded decision, not a unit test.

**Files:**
- Create: `docs/superpowers/specs/2026-06-27-computer-use-cdp-spike-notes.md` (the recorded outcome)

**Interfaces:**
- Consumes: Tasks 1, 2, 3.
- Produces: a decision recorded in the notes file: (a) the exposure path that works (proxy-only or relay), and (b) whether the warm-fork model holds or the start-on-connect fallback is needed. Later tasks read this.

- [ ] **Step 1: Assemble the verification substrate on a KVM host**

Use box2 (docker + KVM) and install Firecracker there the way `.github/workflows/kvm-test.yaml` does (download the pinned `v1.15.0` binary), plus the pinned kernel. Build a Chromium rootfs from the image:

```bash
# On box2: build the image, export its filesystem, assemble an ext4 rootfs with
# the guest agent injected as /init and a /bin/sh present.
docker build -t mitos-computer-use:dev images/computer-use/
cid=$(docker create mitos-computer-use:dev)
mkdir -p /root/cu-root && docker export "$cid" | tar -x -C /root/cu-root
docker rm "$cid"
# Inject the freshly built guest agent as /init (cross-compiled musl agent, the
# same build used for the #316/#318/#320 verification), then mkfs.ext4 + copy.
# (Follow the [[epic-323-agent-integrations-complete]] recipe for the agent build
# and the kvm-test.yaml rootfs assembly for the ext4 packing; size the image to
# the docker rootfs size plus headroom.)
```

Cross-compile `sandbox-server` (with the Task 1 change) for linux/amd64 and the guest agent (musl) as in the prior verification.

- [ ] **Step 2: Boot the standalone server with the Chromium rootfs**

```bash
sandbox-server --addr :8080 \
  --kernel <vmlinux> --rootfs /root/cu-rootfs.ext4 --agent-bin <agent> \
  --firecracker /usr/local/bin/firecracker --data-dir /root/cu-data &
```

- [ ] **Step 3: Create a warm Chromium template (workload + 1 GiB) and confirm CDP at snapshot**

```bash
curl -fsS -X POST localhost:8080/v1/templates -H 'Content-Type: application/json' -d '{
  "id":"chrome",
  "workload":{"command":["/usr/local/bin/start-chromium.sh"],
              "ready":{"port":9222,"path":"/json/version","expect":200,"timeout_seconds":120}},
  "resources":{"vcpu_count":2,"mem_size_mib":1024}
}'
```

Expected: the create returns `ready:true` only after the readiness probe saw CDP answer (the build blocks on it). If it times out, Chromium did not come up; inspect the launcher path and memory.

- [ ] **Step 4: Fork and confirm the child has a live CDP endpoint (snapshot survival)**

```bash
curl -fsS -X POST localhost:8080/v1/fork -d '{"template":"chrome","id":"cu-1"}'
# Open the expose route to the guest CDP port and hit /json/version through it.
curl -fsS http://localhost:8080/v1/sandboxes/cu-1/expose/9222/json/version
```

Expected: a JSON CDP document from the forked child. This proves Chromium survived snapshot/restore. If it does not, record the start-on-connect fallback decision (start Chromium via `exec` after fork) and flag the lost warm-fork benefit back to the issue.

- [ ] **Step 5: Determine the exposure path with a real CDP client**

Tunnel `:8080` to the workstation and run Playwright against the exposed CDP URL:

```python
from playwright.sync_api import sync_playwright
URL = "http://localhost:8080/v1/sandboxes/cu-1/expose/9222"
with sync_playwright() as p:
    browser = p.chromium.connect_over_cdp(URL)
    page = browser.new_page()
    page.goto("https://example.com")
    print(page.title())
    page.screenshot(path="/tmp/cu.png")
    browser.close()
```

If `connect_over_cdp` succeeds with proxy-only behavior, record decision (a) proxy-only. If it fails on the `Host` header or the `ws://127.0.0.1` discovery URL, record decision (b) relay-needed and proceed to Task 5.

- [ ] **Step 6: Record the decision and tear down**

Write `docs/superpowers/specs/2026-06-27-computer-use-cdp-spike-notes.md` with: the exposure decision, the snapshot-survival result, the observed Chromium memory floor, and the exact commands that worked. Tear down the box temp dirs.

```bash
git add docs/superpowers/specs/2026-06-27-computer-use-cdp-spike-notes.md
git commit -s -m "docs(specs): record computer-use CDP spike outcome (#314)"
```

---

### Task 5 (contingency): In-image CDP relay

Execute ONLY if Task 4 step 5 recorded decision (b). If proxy-only worked, skip and note it in the recipe.

**Files:**
- Create: `cmd/cdp-relay/main.go`
- Create: `cmd/cdp-relay/main_test.go`
- Modify: `images/computer-use/Dockerfile` (copy the relay binary, point the launcher at it)
- Modify: `images/computer-use/start-chromium.sh` (run Chromium on an internal port, relay on 9222)

**Interfaces:**
- Produces: a static binary that listens on `:9222`, proxies HTTP to Chromium on `127.0.0.1:9223`, rewrites the host in `/json` and `/json/version` `webSocketDebuggerUrl` / `devtoolsFrontendUrl` to the request's forwarded host, and forwards the WebSocket upgrade with `Host: localhost`.

- [ ] **Step 1: Write the failing test for the discovery rewrite**

```go
func TestRewriteDiscoveryHost(t *testing.T) {
	in := `{"webSocketDebuggerUrl":"ws://127.0.0.1:9223/devtools/browser/ABC"}`
	out := rewriteDiscoveryHost([]byte(in), "example.test", true)
	if !strings.Contains(string(out), "wss://example.test/devtools/browser/ABC") {
		t.Fatalf("host not rewritten: %s", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/cdp-relay/ -run TestRewriteDiscoveryHost -v`
Expected: FAIL (undefined `rewriteDiscoveryHost`).

- [ ] **Step 3: Implement the relay**

Implement `rewriteDiscoveryHost(body []byte, host string, tls bool) []byte` (regex-replace the `ws://127.0.0.1:9223` prefix with `wss://<host>` or `ws://<host>`), and a `main()` that runs an `httputil.ReverseProxy` to `127.0.0.1:9223` with a `ModifyResponse` applying the rewrite for `/json` paths and a `Director` setting `Host: localhost`. Keep it under one file.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/cdp-relay/ -v`
Expected: PASS.

- [ ] **Step 5: Wire the relay into the image and re-verify on box**

Update `start-chromium.sh` to launch Chromium on `--remote-debugging-port=9223` and start the relay on 9222; rebuild the rootfs and repeat Task 4 step 5. Confirm `connect_over_cdp` now works.

- [ ] **Step 6: Commit**

```bash
git add cmd/cdp-relay/ images/computer-use/Dockerfile images/computer-use/start-chromium.sh
git commit -s -m "feat(cdp-relay): normalize CDP discovery + host header for remote exposure (#314)"
```

---

### Task 6: Runnable example `browser_cdp.py`

**Files:**
- Create: `sdk/python/examples/browser_cdp.py`

**Interfaces:**
- Consumes: the proven exposure path from Task 4/5 and the SDK `create`/`fork` with `workload`+`resources` from Task 2.

- [ ] **Step 1: Write the example**

Follow `sdk/python/examples/direct.py` shape (module docstring with the run command, `main(base_url)`, non-zero exit on failure). It creates a warm Chromium template via the SDK (workload + 1 GiB), forks one sandbox, builds the exposed CDP URL, connects with `playwright.chromium.connect_over_cdp`, navigates to a stable page, clicks a known element, screenshots, asserts the page title, and prints `browser_cdp example OK`. Requires `pip install playwright` and `playwright install chromium` for the client side; state that in the docstring.

- [ ] **Step 2: Run the example end to end on the box (against the running server from Task 4)**

Run: `python3 sdk/python/examples/browser_cdp.py http://localhost:8080`
Expected: prints `browser_cdp example OK` and writes a screenshot; exits 0.

- [ ] **Step 3: Commit**

```bash
git add sdk/python/examples/browser_cdp.py
git commit -s -m "docs(examples): drive a Mitos headless Chromium sandbox over CDP (#314)"
```

---

### Task 7: Recipe doc + README row

**Files:**
- Create: `docs/recipes/browser-automation.md`
- Modify: `README.md` (Documentation table, after the agent-harness recipe row near line 392)

**Interfaces:**
- Consumes: the verified commands from Tasks 4 and 6.

- [ ] **Step 1: Write the recipe**

`docs/recipes/browser-automation.md`, paralleling `docs/recipes/agent-harness.md`:
- What it is (a forkable headless-Chromium microVM with a CDP endpoint) and the wedge (self-host, isolation, fork-to-many warm browsers).
- The cluster path: a `SandboxPool` with `spec.template.image`, `resources: {cpu, memory: 1Gi}`, and `workload` (command + ready probe on 9222 `/json/version`).
- The standalone path: `POST /v1/templates` with `workload` + `resources` (the exact body from Task 4 step 3), verified on a KVM host.
- Reaching CDP: the expose route or a Mitos Expose named URL, always auth-gated, never public; the exact `connect_over_cdp` URL form proven in Task 4/5; whether a relay is in the image (per the spike).
- Fork-to-many "best-of-N browsing": warm one template, `fork(n)`, give each child a fresh page; in-flight pages do not transfer across a fork.
- Honest "which path it runs on" note: warm-fork needs the workload (cluster or the standalone passthrough); Chromium needs ~1 GiB; CDP-over-WebSocket only; the VNC desktop surface for vision computer-use agents is a future variant.
- The example: link `sdk/python/examples/browser_cdp.py`.

- [ ] **Step 2: Add the README row**

In `README.md`, after the `Recipe: host an agent harness over HTTP` row:

```markdown
| Recipe: headless Chromium / CDP for browser-automation agents | [docs/recipes/browser-automation.md](docs/recipes/browser-automation.md) |
```

- [ ] **Step 3: Dash scan and commit**

Run: `grep -nP '[\x{2014}\x{2013}]' docs/recipes/browser-automation.md README.md` (expect no output).

```bash
git add docs/recipes/browser-automation.md README.md
git commit -s -m "docs(recipes): headless Chromium / CDP browser-automation recipe (#314)"
```

---

### Task 8: Threat-model delta

**Files:**
- Modify: `docs/threat-model.md`

**Interfaces:**
- Consumes: the security analysis in spec section 6.

- [ ] **Step 1: Add the CDP-exposure row**

Add a row recording: the new surface (an exposed CDP endpoint is full remote control of the browser: file reads, arbitrary JS); the mitigation (the expose access ladder gates it, private by default, never `public` for CDP); and the `--no-sandbox` posture (the Firecracker microVM is the containment boundary, the same model E2B and Browserbase use). Match the table's existing row format and status column.

- [ ] **Step 2: Dash scan and commit**

Run: `grep -nP '[\x{2014}\x{2013}]' docs/threat-model.md` (expect no output).

```bash
git add docs/threat-model.md
git commit -s -m "docs(threat-model): record the CDP-exposure surface for the computer-use template (#314)"
```

---

### Task 9: Publish the image and add CI coverage

**Files:**
- Modify: `.github/workflows/kvm-test.yaml` (a Chromium-template step) OR a new minimal job, sized to what is affordable.
- Possibly: a GHCR publish workflow entry for the new image.

**Interfaces:**
- Consumes: all prior tasks.

- [ ] **Step 1: Publish the image to GHCR**

Build and push `ghcr.io/mitos-run/mitos-computer-use:<version>` (and `:latest`). Record that the new package must be flipped to public once in the GHCR UI (repo convention). Pin the digest in the recipe.

- [ ] **Step 2: Add CI coverage proportional to cost**

Add a `firecracker-test`-style step that builds the image, boots the warm Chromium template, forks, and asserts `/json/version` answers through the expose route. If the Chromium image build is too heavy for the KVM job budget, instead add a lightweight job that builds the image and runs the Task 3 docker smoke (CDP answers `/json/version`), and state explicitly in `docs/recipes/browser-automation.md` that the full warm-fork + Playwright path is box-verified rather than CI-gated. Do not silently drop coverage.

- [ ] **Step 3: Dash scan, lint workflow, commit**

Run: `actionlint .github/workflows/kvm-test.yaml` (if available) and the dash scan.

```bash
git add .github/workflows/kvm-test.yaml
git commit -s -m "ci(kvm): cover the headless Chromium / CDP template (#314)"
```

---

## Self-Review

**Spec coverage:**
- Goal 1 (image): Task 3. Goal 2 (warm-fork wiring): Tasks 1, 4, 7. Goal 3 (exposure): Tasks 4, 5, 7. Goal 4 (example + recipe): Tasks 6, 7. Goal 5 (README + threat-model): Tasks 7, 8. Goal 6 (standalone parity): Tasks 1, 2. Spec section 7 sequencing maps to Tasks 1-9. Open questions 1 and 2 resolved in Task 4; open question 3 resolved in Task 9.
- Non-goals (VNC desktop, stealth, captcha, browser SDK) are not implemented; the recipe notes VNC as a future variant.

**Placeholder scan:** The contingency relay (Task 5) is explicitly conditional on the Task 4 outcome, not a placeholder; its code steps are concrete. `<vmlinux>`, `<agent>`, `<version>`, and `<tag>` are environment-specific values filled at run time, not unspecified logic.

**Type consistency:** `workloadFromReq`/`vmResFromReq` return `*firecracker.WorkloadSpec` / `*firecracker.VMResources` (fields `Command`, `Env`, `Ready{Port,Path,Expect,TimeoutSeconds}`, `VcpuCount`, `MemSizeMib`) consistently across Tasks 1, 2, 4. The CDP port `9222`, the readiness path `/json/version`, and the expose URL form are consistent across Tasks 3, 4, 6, 7.

**Risks carried forward:** Task 4 may flip the design to the relay (Task 5) or to start-on-connect; both are written as explicit, concrete branches, and the start-on-connect fallback returns to the issue because it changes the headline benefit.
