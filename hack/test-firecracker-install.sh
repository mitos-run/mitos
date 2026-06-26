#!/usr/bin/env bash
# Smoke test for the firecracker + jailer install shared by Dockerfile.forkd and
# Dockerfile.husk-stub. Builds a throwaway image that runs hack/install-firecracker.sh
# and asserts BOTH binaries are present and their versions match.
#
# Guards #425: the forkd/husk-stub images previously shipped firecracker but NOT
# the matching jailer, so every pool template build failed at runtime with
# `fork/exec /usr/local/bin/jailer: no such file or directory`.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
img="mitos-fc-install-smoke:test"

docker build -f "$root/test/docker/Dockerfile.firecracker-install" -t "$img" "$root"

docker run --rm "$img" sh -c '
  set -e
  test -x /usr/local/bin/firecracker || { echo "MISSING: /usr/local/bin/firecracker"; exit 1; }
  test -x /usr/local/bin/jailer || { echo "MISSING: /usr/local/bin/jailer"; exit 1; }
  fc=$(firecracker --version | grep -oE "v[0-9]+\.[0-9]+\.[0-9]+" | head -1)
  jl=$(jailer --version | grep -oE "v[0-9]+\.[0-9]+\.[0-9]+" | head -1)
  echo "firecracker=$fc jailer=$jl"
  if [ -z "$fc" ] || [ "$fc" != "$jl" ]; then
    echo "VERSION MISMATCH: firecracker=$fc jailer=$jl"; exit 1
  fi
'

echo "OK: firecracker and jailer both present with matching versions"
