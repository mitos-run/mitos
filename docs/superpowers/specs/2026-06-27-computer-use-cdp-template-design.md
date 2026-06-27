# Computer-use template: headless Chromium + CDP for browser-automation agents

Status: design, approved for spec review.
Issue: #314 (computer-use / VNC desktop template for browser-automation agents).
Depends on the shipped guest port exposure (#228) and preview proxy (#126), and
on the serving-workload primitive (#460) that boots a long-running process so it
is already listening when the template snapshot is taken. The VNC desktop variant
named in the issue is explicitly deferred to a follow-up.

## 1. Summary

Ship a first-party, CI-verified template that boots **headless Chromium with a
Chrome DevTools Protocol (CDP) endpoint** inside a Mitos microVM, exposed over the
existing authenticated preview proxy, plus the wiring that snapshots Chromium
already listening so a `fork` yields N warm browsers in milliseconds.

The scope is deliberately CDP-only. Research into the early-2026 landscape (see
section 8) shows the momentum in agent browser automation is decisively toward
raw CDP over WebSocket: Playwright `connect_over_cdp`, Stagehand v3 (Playwright
removed, straight to CDP), and browser-use (migrated to direct CDP) all target a
Chromium CDP endpoint. A CDP template therefore has the broadest framework
interop, the lightest rootfs, and exposes natively over our HTTP/WebSocket proxy.
The graphical VNC desktop that vision computer-use agents (Anthropic Computer Use,
OpenAI CUA) need is a separate, heavier surface, deferred to its own issue.

The strategic wedge, identical to the rest of Mitos and unavailable from the SaaS
browser clouds (Browserbase, Hyperbrowser; Steel is the only self-host peer and
is a shared-kernel container with no fork): self-host so cookies, sessions, and
scraped data never leave your infrastructure; microVM hardware isolation for
untrusted pages; and fork a warm browser into N isolated attempts at fork(2)
speed.

## 2. Goals and non-goals

Goals:

1. An OCI template image (`images/computer-use`) that boots headless Chromium with
   a CDP endpoint on a known guest port, published to GHCR.
2. Pool / template wiring (`SandboxPool` on a cluster, template on the standalone
   `sandbox-server`) that snapshots Chromium already listening on CDP, so a fork
   gives an instantly warm browser, gated by an HTTP readiness probe.
3. The CDP endpoint reachable from outside over the existing authenticated expose
   proxy, with standard tools (`playwright.chromium.connect_over_cdp`) working
   unmodified.
4. A runnable example driving it end to end (navigate, click, screenshot) and a
   recipe doc with the honest "which path it runs on" note.
5. A README documentation-table entry and a threat-model delta for the new
   CDP-exposure surface.
6. Standalone parity: the standalone `sandbox-server` template endpoint accepts a
   `workload` and `resources` (memory / vcpu) so the warm-fork browser model runs
   on the no-Kubernetes path, not only on a cluster. The engine already supports
   both; only the standalone HTTP handler currently drops them.

Non-goals:

1. A graphical VNC / noVNC desktop (the vision-agent surface). Deferred to a
   follow-up issue; this design notes the seam.
2. Stealth / anti-detect / fingerprint evasion. The base template is a clean,
   standards-compliant Chromium; anti-detect tooling is a gray-area niche the user
   can layer themselves and is out of scope and off-brand.
3. Captcha solving, proxy rotation, or a managed session-replay UI. Those are SaaS
   product features, not a template.
4. A Mitos-specific browser SDK. The value is that existing CDP tools just work.

## 3. Components

Each unit has one purpose and a defined interface.

### 3.1 Template image (`images/computer-use/Dockerfile`)

- Base `debian:stable-slim`. Rationale: it ships `setsid` and `/bin/sh`, both
  required by the serving-workload path (the workload is started in its own
  session via `setsid` so the fork SIGUSR2 broadcast does not terminate it).
- Installs `chromium`, `fonts-liberation`, `ca-certificates`, and a launcher
  script `start-chromium.sh`.
- `start-chromium.sh` runs Chromium headless on guest loopback:
  `chromium --headless=new --remote-debugging-port=9222 --no-sandbox
  --disable-dev-shm-usage --user-data-dir=/data/chrome --remote-debugging-address=127.0.0.1`.
  `--no-sandbox` is correct and intentional here: the microVM is the sandbox
  boundary (the same model E2B Desktop and Browserbase use). This is recorded in
  the threat-model delta, not hidden.
- The guest agent is injected as `/init` by the existing OCI-image-to-rootfs
  pipeline (`internal/fork/imagebuild.go`); the image does not provide its own
  init.
- Published as `ghcr.io/mitos-run/mitos-computer-use:<tag>`. New GHCR packages are
  private by default and need the one-time manual public toggle (existing repo
  convention).

### 3.2 Pool / template wiring

A `SandboxPool` (cluster) or a `POST /v1/templates`-shaped template (standalone)
that sets:

```yaml
spec:
  template:
    image: ghcr.io/mitos-run/mitos-computer-use:<tag>
    resources: { cpu: "2", memory: "1Gi" }     # Chromium needs ~1Gi
    workload:
      command: ["/usr/local/bin/start-chromium.sh"]
      ready: { port: 9222, path: /json/version, expect: 200 }
```

The `ready` probe makes the template build block until CDP answers HTTP 200 on
`/json/version`, so the snapshot always captures a live CDP endpoint. The
`resources` override raises the default 512 MiB to 1 GiB for Chromium.

### 3.3 CDP exposure

CDP is HTTP plus a WebSocket upgrade, so it rides the existing expose paths, all
WebSocket-safe (`FlushInterval: -1`, raw-byte vsock tunnel):

- standalone: `GET /v1/sandboxes/{id}/expose/9222/...`
- cluster named URL: `https://<label>.<expose-domain>/...` via Mitos Expose
- local: the loopback host-forward (`POST /v1/sandboxes/{id}/forward`)

Open technical decision, resolved by the spike in section 7: Chrome advertises a
`ws://127.0.0.1:9222/...` URL in `/json/version` and enforces a `Host: localhost`
check on the WS upgrade. Two candidate resolutions:

- **(a) Proxy normalization only.** Normalize the upstream `Host` header to
  `localhost` at the expose proxy and rely on the CDP client rewriting the
  discovery host to the endpoint it was given (Playwright's documented behavior;
  to be confirmed empirically). Zero new code if it holds.
- **(b) A small CDP relay in the image.** A static binary on `:9222` that proxies
  to Chromium on an internal port, rewrites the `/json/*` discovery doc to the
  externally reachable host, and sets `Host: localhost` on the upstream WS dial.
  Bulletproof and matches what Steel and Browserbase do; costs one small binary.

Decision rule: prefer (a); fall back to (b) only if the spike shows a standard
`connect_over_cdp` against the exposed URL does not work with proxy-only host
normalization. The recipe and example only ever describe the path that is proven
to work.

### 3.4 Standalone sandbox-server workload + resources passthrough

The serving-workload primitive (#460) and per-template memory sizing are wired on
the cluster path only: `cmd/sandbox-server`'s `POST /v1/templates` decodes just
`{id, init_wait_seconds, network}` (`main.go:414`) and calls
`engine.CreateTemplate(req.ID, s.rootfsPath, nil, nil, nil, nil)` (`main.go:476`),
dropping the `workload` and `vmRes` parameters the engine already accepts
(`engine.go:2005`). The standalone build is therefore hardcoded to 512 MiB / 1
vCPU with no running workload, which is too small for Chromium and cannot
snapshot it live.

This task extends the standalone handler to parity with the engine:

- Add `workload` and `resources` to the `createTemplateReq` struct, mapping JSON
  to `firecracker.WorkloadSpec` (command, env, ready probe) and
  `firecracker.VMResources` (vcpu, mem MiB).
- Pass them through:
  `engine.CreateTemplate(req.ID, s.rootfsPath, nil, nil, workload, vmRes)`.
- The Python SDK `DirectSandbox` template-create path gains optional `workload`
  and `resources` arguments so the example and recipe can drive it.

This unlocks the warm-fork browser model on the no-Kubernetes self-host path and
makes the whole feature verifiable end to end on a single KVM host (box1) with the
standalone server, the same harness used for the #316/#318/#320 verification.

### 3.5 Example and recipe

- `sdk/python/examples/browser_cdp.py`:
  `playwright.chromium.connect_over_cdp(<exposed-cdp-url>)`, then `goto`, `click`,
  `screenshot`, and assert a marker, following the existing
  `sdk/python/examples/direct.py` shape (docstring, `main()`, non-zero exit on
  failure so CI gates).
- `docs/recipes/browser-automation.md`: the pool YAML, the standalone path, the
  example walkthrough, the fork-to-many "best-of-N browsing" pattern, and the
  honest "which path it runs on" note (cluster husk pool or standalone
  sandbox-server; ~1 GiB RAM; CDP-over-WebSocket only; VNC desktop is a future
  variant). Parallels `docs/recipes/agent-harness.md`.

### 3.6 README and threat-model

- README documentation table: a row "Recipe: headless Chromium / CDP for
  browser-automation agents" linking the recipe, satisfying the issue comment's
  README follow-up.
- `docs/threat-model.md`: a row for the CDP-exposure surface (see section 6).

## 4. Data flow

Build time: controller / sandbox-server pulls the image, extracts it, injects the
agent as `/init`, boots the microVM, the agent runs `start-chromium.sh` as a
detached session workload, the build blocks on the `ready` probe hitting
`/json/version`, then pauses and snapshots with Chromium live.

Claim / fork time: a fork restores the snapshot; each child has its own Chromium
process state and its own loopback CDP listener. In-flight pages do not transfer
across a fork (the child resumes snapshot state, not live connections); a client
connects fresh and opens a new page or context. This matches the documented
agent-harness fork semantics.

Drive time: client -> expose proxy (auth-gated) -> vsock PortForward tunnel ->
guest `127.0.0.1:9222` -> Chromium CDP. Playwright / Stagehand / browser-use
connect over CDP and drive the browser.

## 5. Error handling and failure behavior

- Readiness probe timeout: if Chromium does not answer `/json/version` within the
  probe `timeoutSeconds`, the template build fails with the existing workload
  build error (actionable remediation: check the launcher logs, raise memory).
  No silent half-built snapshot.
- Out-of-memory: Chromium under-provisioned (below ~1 GiB) is the most likely
  failure; the recipe states the requirement and the pool sets it. Documented,
  not implied.
- Fork restore: a child where Chromium did not survive snapshot/restore must fail
  closed on the readiness re-check rather than serve a dead CDP endpoint. The
  spike validates restore behavior (section 7); if Chromium does not survive
  restore reliably, the fallback is start-on-first-connect (which loses the warm
  fork advantage) and that trade-off is brought back to the issue, not shipped
  silently.
- Auth: the exposed CDP endpoint is gated by the expose access ladder, private by
  default; an unauthenticated request gets the standard expose 401/403, never a
  live CDP connection.

## 6. Security and threat model

The new surface is an exposed CDP endpoint. CDP is full remote control of the
browser: it can navigate `file://`, read local files, and execute arbitrary JS in
pages. Therefore:

- The exposed endpoint MUST remain behind the expose access ladder, private by
  default; the recipe never demonstrates the `public` tier for CDP.
- `--no-sandbox` disables Chromium's own sandbox; the containment boundary is the
  Firecracker microVM, not Chrome's seccomp/namespace sandbox. This is the same
  posture as E2B and Browserbase and is stated plainly.
- Rendering untrusted web content is the intended isolation story: a hostile page
  is confined to the microVM and cannot reach the host, control plane, or sibling
  sandboxes.

`docs/threat-model.md` gains a row recording the CDP-exposure surface, the
auth-gating requirement, and the microVM-as-sandbox rationale, in the same PR.

## 7. Implementation sequencing

1. **CDP-remote-exposure spike.** Build the image locally, boot it on a KVM host
   (box1), expose CDP through the proxy, and determine whether proxy-only host
   normalization (3.3a) lets `connect_over_cdp` work, or whether the relay (3.3b)
   is needed. This decides the image and proxy surface before anything is written
   down. Also validates that Chromium survives snapshot/restore across a fork.
2. Standalone workload + resources passthrough (section 3.4): the engine already
   supports them; this wires the standalone handler and the SDK so box1 can
   verify the warm-fork model without a cluster.
3. Image + launcher, built and pushed to GHCR.
4. Pool / template wiring with the workload and resources.
5. Exposure path finalized per the spike outcome.
6. Example + recipe, verified end to end on box1 (navigate, click, screenshot).
7. README row + threat-model delta.
8. CI: a `firecracker-test`-style step if affordable (the Chromium image build is
   heavy); otherwise the box1 run is the authoritative end-to-end proof and the
   doc is explicit about what is CI-gated versus box1-verified, honoring the
   no-unverified-claims rule.

## 8. Landscape and positioning (research basis)

From an early-2026 research pass (sources cited in the PR):

- The dominant programmatic surface is CDP over WebSocket. Playwright
  `connect_over_cdp` is Chromium-only; Stagehand v3 removed Playwright for raw CDP
  (cited 44% faster on shadow DOM/iframes); browser-use migrated to direct CDP
  (WebVoyager 89.1%). DOM-driven (CDP) stacks are 12-17 points more reliable than
  vision stacks on common tasks.
- Vision computer-use agents (Anthropic Computer Use, OpenAI CUA) instead need a
  graphical screen (Xvfb + VNC + xdotool); E2B Desktop is exactly that (Xfce +
  noVNC on 6080). That is the deferred VNC variant, a different and smaller
  segment.
- Self-host peers: Steel is open-source and self-hostable but a shared-kernel
  container with no fork; Browserbase and Hyperbrowser are SaaS-only. None fork a
  warm browser at fork(2) speed, and none keep your data on your own infra with
  microVM isolation.
- Stealth / anti-detect (Camoufox, Patchright, undetected-chromedriver) is a
  scraping arms race and a gray area; the base template stays clean and
  standards-compliant, leaving that to the user to layer.
- Distribution norm: flagship browser surfaces ship first-party as a repo plus a
  template image (E2B Desktop, Steel). Mitos ships this in-repo, CI-verified, for
  the same reason; a community `awesome-mitos` discovery list is a separate, later
  initiative, not where the canonical template lives.

## 9. Open questions

1. Spike outcome: proxy-only host normalization (3.3a) versus an in-image CDP
   relay (3.3b). Resolved in implementation step 1.
2. Whether Chromium survives VM snapshot/restore cleanly enough to keep the
   warm-fork model, or whether start-on-first-connect is the honest fallback
   (section 5). Resolved in implementation step 1; a fallback changes the
   headline benefit and comes back to the issue.
3. Whether the Chromium image build is affordable in CI or stays a box1-gated
   verification (step 7).
