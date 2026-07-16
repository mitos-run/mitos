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

# Chromium is pinned to an exact version on purpose, and the pin has history.
# 150.0.7871.46-1~deb13u1 SIGTRAPed on launch (TRAP_BRKPT from Chromium's own
# IMMEDIATE_CRASH, no diagnostic flushed) and never bound the CDP port, so the
# browser template did not start at all: reproduced on amd64 (CI) and arm64,
# independent of --no-sandbox, the seccomp profile, and every headless/gpu flag
# combination tried. We pinned back to 147.0.7727.137 until Debian shipped a
# launchable 150+ (issue tracked in the PR that added the pin). That 147 build
# has since been REMOVED from the archive (apt exit 100 on every build). The
# security pocket keeps only its latest build, so the pin has to track it: it
# shipped 150.0.7871.114, then rotated that out too (apt exit 100 again), and
# now ships 150.0.7871.124, ten point releases further on and still well past
# the SIGTRAP build. The docker-build CDP smoke (below in ci.yaml) is the gate
# that proves it launches and binds the debugging port before this can merge.
# The isolation boundary here remains the Firecracker microVM, not Chromium's
# own sandbox (already --no-sandbox; see docs/threat-model.md). apt-mark hold
# keeps a transitive upgrade from moving the version underneath the pin.
ARG CHROMIUM_VERSION=150.0.7871.124-1~deb13u1
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
