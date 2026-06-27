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

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      chromium \
      fonts-liberation \
      ca-certificates \
 && rm -rf /var/lib/apt/lists/*

COPY --from=relay-build /cdp-relay /usr/local/bin/cdp-relay
COPY images/computer-use/start-chromium.sh /usr/local/bin/start-chromium.sh
RUN chmod +x /usr/local/bin/start-chromium.sh /usr/local/bin/cdp-relay

# CDP endpoint, served by the relay. Reach it over the Mitos expose proxy, never
# publicly.
EXPOSE 9222
