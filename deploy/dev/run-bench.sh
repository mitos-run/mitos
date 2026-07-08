#!/bin/sh
# Build + run the in-process fork bench inside the mitos-forkbench pod.
# Drive: kubectl -n mitos exec deploy/mitos-forkbench -- sh /src/deploy/dev/run-bench.sh
# Override iterations/mode/template via env: ITERS=20 sh run-bench.sh
set -e
cd /src
go build -o /tmp/bench ./cmd/bench
echo "=== forkbench: $(git rev-parse --short HEAD) ==="
exec /tmp/bench \
  --mode "${MODE:-fork-exec}" \
  --data-dir /var/lib/mitos \
  --template "${TEMPLATE:-python}" \
  --firecracker /fc/firecracker \
  --iterations "${ITERS:-6}" \
  --warmup "${WARMUP:-2}" \
  --allow-unverified-snapshots
