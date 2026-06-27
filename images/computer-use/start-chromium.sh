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
