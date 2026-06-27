# Headless Chromium / CDP for browser-automation agents

Give an agent a forkable, hardware-isolated browser. This recipe boots headless
Chromium with a Chrome DevTools Protocol (CDP) endpoint inside a Mitos microVM,
snapshots it already listening so a fork wakes a warm browser in milliseconds, and
exposes CDP so Playwright, Stagehand, browser-use, or any CDP client drives it.

Why Mitos for this, versus a SaaS browser cloud: your cookies, sessions, and
scraped data stay on your own infrastructure; a hostile page is contained in a
Firecracker microVM, not a shared-kernel container; and you can fork one warm
browser into N isolated attempts at fork(2) speed (best-of-N browsing).

## The image

`images/computer-use` is a debian-slim image with Chromium, the CDP relay, and a
launcher. The launcher starts Chromium headless on an internal loopback port and
the relay on the exposed port 9222. Build and publish it (the build context is the
repository root, because the relay is compiled from source):

```bash
docker build -f images/computer-use/Dockerfile -t ghcr.io/mitos-run/mitos-computer-use:dev .
```

Chromium runs with `--no-sandbox`: the microVM is the isolation boundary, not
Chromium's own sandbox (the same posture as E2B Desktop and Browserbase). See
[the threat model](../threat-model.md).

## On a Kubernetes cluster

A `SandboxPool` whose template runs Chromium as a serving workload, so the pool
snapshots it already listening on CDP and every claim or fork wakes warm:

```yaml
apiVersion: mitos.run/v1
kind: SandboxPool
metadata:
  name: browser
spec:
  template:
    image: ghcr.io/mitos-run/mitos-computer-use:dev
    resources: { cpu: "2", memory: "1536Mi" }   # Chromium needs more than 512Mi
    workload:
      command: ["/usr/local/bin/start-chromium.sh"]
      ready: { port: 9222, path: /json/version, expect: 200, timeoutSeconds: 90 }
```

## On the standalone sandbox-server (no Kubernetes)

Run a real-mode `sandbox-server` against the computer-use rootfs, then create a
warm template with the workload and resources (the standalone template endpoint
accepts both):

```bash
curl -fsS -X POST localhost:8080/v1/templates -H 'Content-Type: application/json' -d '{
  "id": "browser",
  "workload": {"command": ["/usr/local/bin/start-chromium.sh"],
               "ready": {"port": 9222, "path": "/json/version", "expect": 200, "timeout_seconds": 90}},
  "resources": {"vcpu_count": 2, "mem_size_mib": 1536}
}'
```

The SDK accepts the same fields: `mitos.create("browser", workload=..., resources=...)`.

## Reaching CDP

CDP is HTTP plus a WebSocket upgrade. Two ways to reach guest port 9222:

- **Loopback host-forward** (local and SDK): open a forward and connect a CDP
  client to the returned `host:port`. This is what the example uses and works
  against any standalone server with no extra setup.
- **Named URL over the preview proxy** (authenticated, shareable): the in-image
  CDP relay makes this work. Chromium's DevTools server rejects any `Host` that is
  not an IP or `localhost`, so the relay sends `Host: localhost` upstream and
  rewrites the discovery `webSocketDebuggerUrl` to the external origin (from the
  `X-Forwarded-Host` the expose proxy now sets). Reach it with
  `sandbox.get_host(9222)` (a signed, expiring URL) and point your CDP client at
  it. Keep this endpoint private; CDP is full remote control of the browser.

Drive it with Playwright (the same shape works for Stagehand and browser-use):

```python
from playwright.sync_api import sync_playwright
with sync_playwright() as p:
    browser = p.chromium.connect_over_cdp(cdp_url)   # forward host:port or get_host URL
    page = browser.new_page()
    page.goto("https://example.com")
    page.screenshot(path="shot.png")
    browser.close()
```

The runnable end-to-end example is
[`sdk/python/examples/browser_cdp.py`](../../sdk/python/examples/browser_cdp.py).

## Fork to many: best-of-N browsing

Warm one browser, fork it into N isolated microVMs, give each its own attempt:

```python
base = mitos.create("browser", workload=..., resources={"vcpu_count": 2, "mem_size_mib": 1536})
children = base.fork(8)   # 8 warm browsers, each restored from the snapshot
# drive each child's CDP independently; a crash or hostile page in one cannot
# reach its siblings, the host, or the control plane.
```

In-flight pages do not transfer across a fork (a child resumes snapshot state, not
live CDP connections); connect fresh and open a new page or context after a fork.

## Which path runs this, honestly

- The warm-fork model needs the serving workload, which runs on a cluster
  `SandboxPool` and on the standalone sandbox-server (both wire the `workload` and
  `resources` fields). Chromium needs roughly 1.5 to 2 GiB; 512 MiB is too small.
- CDP is exposed over WebSocket. A full graphical desktop for vision computer-use
  agents (Anthropic Computer Use, OpenAI CUA) over VNC / noVNC is a separate,
  heavier surface and is not part of this template.
- Verified on real KVM: a Chromium serving workload reaches Ready and snapshots
  live; a fork wakes a warm browser in about 2.7 seconds; Playwright drives it
  over CDP on both the host-forward and the relay paths (navigate, click,
  screenshot). See `docs/superpowers/specs/2026-06-27-computer-use-cdp-spike-notes.md`.
