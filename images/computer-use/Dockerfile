# Headless Chromium + CDP template for Mitos browser-automation sandboxes (#314).
# The microVM is the isolation boundary; Chromium runs with --no-sandbox.
#
# The build context is the repository root (the relay is compiled from source):
#   docker build -f images/computer-use/Dockerfile -t mitos-computer-use .

# Builder: compile the CDP relay (standard library only) from source. The relay
# presents CDP on the exposed port, sends the upstream host:port as Host so Chromium's
# DevTools host-header check passes behind the named-URL expose proxy, and
# rewrites the discovery webSocketDebuggerUrl to the external origin.
FROM golang:1.26-bookworm AS relay-build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/cdp-relay ./cmd/cdp-relay
RUN CGO_ENABLED=0 GOFLAGS=-mod=mod go build -o /cdp-relay ./cmd/cdp-relay

# Runtime: debian-stable-slim ships setsid and /bin/sh, both required by the
# serving-workload path that snapshots Chromium already listening.
FROM debian:stable-slim

# Chromium is pinned to 147, from debian stable/main, on purpose. The
# debian-security rebuild 150.0.7871.46-1~deb13u1 SIGTRAPs on launch
# (TRAP_BRKPT from Chromium's own IMMEDIATE_CRASH, no diagnostic flushed) and
# never binds the CDP debugging port, so the browser template does not start at
# all: reproduced on amd64 (CI) and arm64, independent of --no-sandbox, the
# seccomp profile, and every headless/gpu flag combination tried. 147 launches
# clean. The pin is a downgrade past a security update, but the isolation
# boundary here is the Firecracker microVM, not Chromium's own sandbox (already
# --no-sandbox; see docs/threat-model.md), so the exposure delta is bounded to
# the already-untrusted guest. apt-mark hold keeps the security pocket from
# pulling 150 back in transitively. Revert to the security build once Debian
# ships a launchable 150+: tracked in the issue referenced in the PR.
ARG CHROMIUM_VERSION=147.0.7727.137-1~deb13u1
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      chromium="${CHROMIUM_VERSION}" \
      chromium-common="${CHROMIUM_VERSION}" \
      chromium-sandbox="${CHROMIUM_VERSION}" \
      fonts-liberation \
      ca-certificates \
 && apt-mark hold chromium chromium-common chromium-sandbox \
 && rm -rf /var/lib/apt/lists/*

COPY --from=relay-build /cdp-relay /usr/local/bin/cdp-relay
COPY images/computer-use/start-chromium.sh /usr/local/bin/start-chromium.sh
RUN chmod +x /usr/local/bin/start-chromium.sh /usr/local/bin/cdp-relay

# CDP endpoint, served by the relay. Reach it over the Mitos expose proxy, never
# publicly.
EXPOSE 9222
