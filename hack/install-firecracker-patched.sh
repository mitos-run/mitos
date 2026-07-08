#!/usr/bin/env bash
# Swap in the Mitos-patched Firecracker VMM (live-fork: m1 memfd/MAP_SHARED guest
# memory + m2 UFFD write-protect + m4b restore-side arm + m5 child memfd import +
# m6a vmstate-only snapshot) over the STOCK firecracker that
# hack/install-firecracker.sh installed, while leaving the stock jailer untouched.
#
# Why this is a safe, always-on swap (NOT the live-fork wiring, that is m4b):
#   - The patched binary is behaviour-identical to stock Firecracker v1.15.0
#     UNLESS FIRECRACKER_MITOS_SHARED_MEM, FIRECRACKER_MITOS_WP_UDS or
#     FIRECRACKER_MITOS_CHILD_MEMFD are set at runtime, OR a PUT /snapshot/create
#     sends snapshot_type "MitosVmstateOnly". Full and Diff snapshots are
#     byte-for-byte unchanged, so the image behaves like stock until those gates are
#     used. Installing it everywhere is safe.
#   - Provenance: built reproducibly in mitos-run/firecracker on branch
#     mitos/child-memfd-import-v1.15.0 via .github/workflows/build-patched-fc.yml
#     (green run https://github.com/mitos-run/firecracker/actions/runs/28911590590)
#     and published as a GitHub release asset, pinned by sha256 below. A compromised
#     CDN or a network substitution cannot install a different binary. This binary
#     adds the m5 child-side memfd import (issue #832): a co-located fork child that
#     is launched with FIRECRACKER_MITOS_CHILD_MEMFD boots its guest RAM by mapping
#     the source guest memfd MAP_PRIVATE (copy-on-write) plus the frozen overlay and
#     loads NO disk mem file, so the vmstate-only fork drops the create_snapshot mem
#     write end to end. It also keeps the m4b restore-side fix: a RESTORED source VM
#     backs its guest RAM with a shared memfd, exports it, and offers write-protect
#     during restore, so the live-cow fork arms on a restored source, not only a
#     booted one.
#   - Revert = drop the COPY + RUN that invokes this script from Dockerfile.forkd
#     and Dockerfile.husk-stub (and the smoke-test fixture); the stock firecracker
#     from hack/install-firecracker.sh then remains in place.
#
# MUST run AFTER hack/install-firecracker.sh (which installs the stock jailer and
# a stock firecracker at /usr/local/bin). Requires curl, ca-certificates, and
# coreutils (sha256sum), which the caller installs.
set -euo pipefail

# --- single pinned provenance constants (audit + bump only here) -------------
PATCHED_FC_VERSION="v1.15.0"
PATCHED_FC_URL="https://github.com/mitos-run/firecracker/releases/download/mitos-fc-child-memfd-import-v1.15.0/firecracker-v1.15.0-x86_64-mitos-child-memfd-import"
PATCHED_FC_SHA256="f36bba0c663f9c38276dc1d83eda8e117d98aca7a50ac274d879556766cc2421"
# -----------------------------------------------------------------------------

arch="$(uname -m)"
if [ "$arch" != "x86_64" ]; then
    echo "install-firecracker-patched: no patched Firecracker for $arch; only x86_64 is built by mitos-run/firecracker ci/build-patched-fc. Add the arch's release asset + pinned sha256 before building it." >&2
    exit 1
fi

if [ ! -x /usr/local/bin/firecracker ]; then
    echo "install-firecracker-patched: stock firecracker missing; run hack/install-firecracker.sh first" >&2
    exit 1
fi
if [ ! -x /usr/local/bin/jailer ]; then
    echo "install-firecracker-patched: jailer missing; run hack/install-firecracker.sh first" >&2
    exit 1
fi

bin="$(mktemp)"
trap 'rm -f "$bin"' EXIT

curl -fsSL --connect-timeout 30 --max-time 300 --retry 3 --retry-connrefused \
    -o "$bin" "$PATCHED_FC_URL"
# Verify the download against the pinned digest BEFORE installing anything.
echo "${PATCHED_FC_SHA256}  ${bin}" | sha256sum -c -

install -m 0755 "$bin" /usr/local/bin/firecracker

# Re-assert the swap: the installed firecracker must report the expected version
# and still match the (untouched, stock) jailer so forkd's jailed launch keeps
# working. Same version string as stock, so any version-match check still passes.
fc_ver="$(/usr/local/bin/firecracker --version | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' | head -1)"
jl_ver="$(/usr/local/bin/jailer --version | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' | head -1)"
if [ "$fc_ver" != "$PATCHED_FC_VERSION" ]; then
    echo "install-firecracker-patched: patched firecracker reports '$fc_ver', expected '$PATCHED_FC_VERSION'" >&2
    exit 1
fi
if [ "$fc_ver" != "$jl_ver" ]; then
    echo "install-firecracker-patched: version mismatch firecracker=$fc_ver jailer=$jl_ver" >&2
    exit 1
fi
# Belt and braces: confirm the on-disk binary is exactly the pinned artifact.
echo "${PATCHED_FC_SHA256}  /usr/local/bin/firecracker" | sha256sum -c -
echo "install-firecracker-patched: installed Mitos-patched firecracker ${fc_ver} (x86_64); jailer left stock. Behaviour-identical to stock until FIRECRACKER_MITOS_* env is set (m1/m2/m4b source arm, m5 child memfd import) or a MitosVmstateOnly snapshot is requested (m6a)."
