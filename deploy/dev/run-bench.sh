#!/bin/sh
# Build + run the in-process fork bench inside the mitos-forkbench pod.
# Drive: kubectl -n mitos exec deploy/mitos-forkbench -- sh /src/deploy/dev/run-bench.sh
# Override iterations/mode/template via env: ITERS=20 sh run-bench.sh
set -e
# Stage the read-only prod template into the writable data-dir once (Firecracker
# opens the fork rootfs read-write; the prod template mount stays untouched).
TMPL="${TEMPLATE:-python}"
if [ ! -f "/var/lib/mitos/templates/$TMPL/snapshot/vmstate" ]; then
  echo "staging template copy for $TMPL (one-time)..."
  mkdir -p "/var/lib/mitos/templates/$TMPL"
  cp -a /template-src/. "/var/lib/mitos/templates/$TMPL/"
fi
cd /src
go build -o /tmp/bench ./cmd/bench
echo "FORKBENCH_RESULT_START $(git rev-parse --short HEAD)"
/tmp/bench \
  --mode "${MODE:-fork-exec}" \
  --data-dir /var/lib/mitos \
  --template "${TEMPLATE:-python}" \
  --firecracker /fc/firecracker \
  --iterations "${ITERS:-6}" \
  --warmup "${WARMUP:-2}" \
  --summary \
  --allow-unverified-snapshots 2>/tmp/fc.log
echo "FORKBENCH_RESULT_END"
