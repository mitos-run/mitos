#!/bin/sh
# Start headless Chromium plus the CDP relay. Run as the serving workload so the
# template snapshot captures both already listening and a fork wakes warm. The
# microVM is the isolation boundary, so Chromium's own sandbox is disabled
# (--no-sandbox); see docs/threat-model.md.
#
# Chromium binds CDP on an internal loopback port (9223); the relay owns the
# exposed port (9222). The relay sends the upstream host:port as Host so Chromium's
# DevTools host-header check passes behind the named-URL expose proxy, and
# rewrites the discovery webSocketDebuggerUrl to the external origin.
set -eu
mkdir -p /data/chrome
chromium \
  --headless=new \
  --no-sandbox \
  --disable-dev-shm-usage \
  --disable-gpu \
  --remote-debugging-address=127.0.0.1 \
  --remote-debugging-port=9223 \
  --user-data-dir=/data/chrome \
  about:blank &
exec /usr/local/bin/cdp-relay --listen :9222 --upstream 127.0.0.1:9223
