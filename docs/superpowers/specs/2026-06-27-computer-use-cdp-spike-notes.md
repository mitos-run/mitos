# Computer-use CDP template: spike outcome (#314, Task 4)

Verified on box2 (bare-metal KVM, Firecracker v1.15.0, kernel 6.1.155), with the
standalone `sandbox-server` carrying the Task 1 workload+resources passthrough, a
debian-stable-slim + chromium rootfs, and the freshly built Rust guest agent.

## Resolved open questions

1. **Does Chromium survive snapshot/restore (the warm-fork model)? YES.**
   A serving-workload template that launches headless Chromium (workload command
   `/usr/local/bin/start-chromium.sh`, ready probe `9222 /json/version`) reaches
   Ready and snapshots Chromium live in ~17.5s. A fork restores a warm browser in
   ~2.7s whose CDP endpoint is live immediately (no cold start), and a Playwright
   `connect_over_cdp` client drives it end to end: navigate, click, observe the
   DOM update, screenshot. The warm-fork headline benefit holds; the
   start-on-connect fallback is NOT needed.

2. **Exposure path: forward works with zero new code; the named-URL HTTP proxy
   needs a relay.**
   - The loopback host-forward (`POST /v1/sandboxes/{id}/forward`, raw TCP) works
     perfectly: `/json/version` returns CDP, and Chromium reflects the request
     `Host` into `webSocketDebuggerUrl`, so Playwright `connect_over_cdp` against
     the forwarded `127.0.0.1:NNNNN` works unmodified. This covers the SDK / local
     / self-host case.
   - The HTTP expose reverse proxy (`/v1/sandboxes/{id}/expose/9222/...`, the
     named-URL / preview-proxy path) returns `500: Host header is specified and is
     not an IP address or localhost`. This is Chromium's DevTools host-header
     check rejecting the proxied `Host`. There is no single `Host` value that is
     both accepted by Chromium (IP or `localhost`) and usable by a remote client
     (the external hostname), so a CDP-aware relay that (a) sends `Host: localhost`
     upstream and (b) rewrites the `/json/*` discovery `webSocketDebuggerUrl` to
     the external origin is required for the named-URL path. Decision: relay needed
     for named-URL CDP; the forward path needs none.

## Bug found and fixed during the spike

The #460 serving-workload readiness probe sent `GET <path> HTTP/1.0`. Chromium's
DevTools HTTP server rejects HTTP/1.0 (`Cannot handle request with protocol:
HTTP/1.0`) and never returns 200, so the gate timed out against a Chromium that
was listening, failing the build. Fixed in `guest/agent-rs/src/service/workload.rs`
(HTTP/1.0 -> HTTP/1.1, `Connection: close` retained) with a contract test, commit
`1f25a9d`. This is a general #460 fix, not browser-specific.

## Sizing

- 512 MiB (the standalone default) is far too small; 1024 MiB triggers V8
  CodeRange OOM noise in a child renderer (CDP still binds). 1536 MiB builds and
  serves cleanly; the recipe and pool set ~1.5 to 2 GiB via `resources`.
- The debian-slim + chromium rootfs is ~766 MiB extracted; the template uses a
  2.5 GiB ext4.

## Incidental notes

- The agent `mount /dev: Resource busy` line is benign: init is non-fatal by
  design and the kernel already auto-mounts devtmpfs.
- box2 root disk is only 59 GiB and a co-resident husk stack holds ~14 GiB; each
  warm template costs ~2.5 GiB rootfs + ~1.5 GiB mem snapshot, so the CI / repro
  story must budget disk or reuse one template.
