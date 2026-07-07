#!/usr/bin/env bash
# Smoke test for the firecracker + jailer install shared by Dockerfile.forkd and
# Dockerfile.husk-stub. Builds a throwaway image that runs hack/install-firecracker.sh
# followed by hack/install-firecracker-patched.sh and asserts BOTH binaries are
# present, their versions match, AND the firecracker on disk is exactly the pinned
# Mitos-patched artifact.
#
# Guards #425: the forkd/husk-stub images previously shipped firecracker but NOT
# the matching jailer, so every pool template build failed at runtime with
# `fork/exec /usr/local/bin/jailer: no such file or directory`.
# Also guards the patched swap: firecracker must be the pinned patched binary while
# the jailer stays stock (live-fork is runtime-gated; see install-firecracker-patched.sh).
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
img="mitos-fc-install-smoke:test"

# Single source of truth for the pinned Mitos-patched firecracker sha256: read it
# straight out of hack/install-firecracker-patched.sh so a version bump only has to
# touch that one file and this test cannot drift from what the images actually install.
PATCHED_FC_SHA256="$(grep -oE 'PATCHED_FC_SHA256="[0-9a-f]{64}"' "$root/hack/install-firecracker-patched.sh" | grep -oE '[0-9a-f]{64}')"
if [ -z "$PATCHED_FC_SHA256" ]; then
  echo "could not read PATCHED_FC_SHA256 from hack/install-firecracker-patched.sh" >&2
  exit 1
fi

# Build/run for linux/amd64: the Mitos-patched firecracker is x86_64-only (as are
# the forkd/husk-stub images, whose rust agent targets x86_64-unknown-linux-musl),
# so on an arm64 dev host this exercises the same arch the images ship.
docker build --platform linux/amd64 -f "$root/test/docker/Dockerfile.firecracker-install" -t "$img" "$root"

docker run --platform linux/amd64 --rm -e PATCHED_FC_SHA256="$PATCHED_FC_SHA256" "$img" sh -c '
  set -e
  test -x /usr/local/bin/firecracker || { echo "MISSING: /usr/local/bin/firecracker"; exit 1; }
  test -x /usr/local/bin/jailer || { echo "MISSING: /usr/local/bin/jailer"; exit 1; }
  fc=$(firecracker --version | grep -oE "v[0-9]+\.[0-9]+\.[0-9]+" | head -1)
  jl=$(jailer --version | grep -oE "v[0-9]+\.[0-9]+\.[0-9]+" | head -1)
  echo "firecracker=$fc jailer=$jl"
  if [ -z "$fc" ] || [ "$fc" != "$jl" ]; then
    echo "VERSION MISMATCH: firecracker=$fc jailer=$jl"; exit 1
  fi
  echo "${PATCHED_FC_SHA256}  /usr/local/bin/firecracker" | sha256sum -c - || {
    echo "PATCHED SHA MISMATCH: /usr/local/bin/firecracker is not the pinned patched binary"; exit 1
  }
'

echo "OK: patched firecracker (pinned sha) and stock jailer both present with matching versions"
