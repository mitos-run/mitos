#!/usr/bin/env bash
# Install the Firecracker VMM AND its matching jailer into /usr/local/bin.
#
# The firecracker release tarball ships both binaries (firecracker-<v> and
# jailer-<v>); they MUST be installed together and at the same version. forkd
# launches every VM through the jailer (the secure default, #352), so an image
# that ships firecracker without the jailer fails every pool template build at
# runtime with `fork/exec /usr/local/bin/jailer: no such file or directory`
# (#425). This is the single install path shared by Dockerfile.forkd and
# Dockerfile.husk-stub so the two cannot drift.
#
# Requires curl, ca-certificates, and coreutils (sha256sum) to be present already
# (the caller installs them).
set -euo pipefail

FC_VERSION="${FC_VERSION:-v1.15.0}"

# uname -m returns the kernel arch (x86_64 / aarch64 on Linux), not the Go/Docker
# platform names (amd64 / arm64), so only these two are matched.
case "$(uname -m)" in
    x86_64) arch="x86_64" ;;
    aarch64) arch="aarch64" ;;
    *) echo "install-firecracker: unsupported arch $(uname -m)" >&2; exit 1 ;;
esac

# Pinned tarball SHA256 per arch (supply-chain defense): a compromised CDN or a
# network substitution cannot install a different binary that would still pass the
# version-match check. These digests are tied to FC_VERSION; bumping the version
# REQUIRES updating both, taken from the official firecracker release
# `firecracker-<v>-<arch>.tgz.sha256.txt` asset.
case "${FC_VERSION}_${arch}" in
    v1.15.0_x86_64)  sha="00cadf7f21e709e939dc0c8d16e2d2ce7b975a62bec6c50f74b421cc8ab3cab4" ;;
    v1.15.0_aarch64) sha="58325e6c3c539482a412ec0b60e6f539c3320adebcf8179c7629d06736aee0bd" ;;
    *) echo "install-firecracker: no pinned sha256 for ${FC_VERSION} ${arch}; add it from the firecracker release .sha256.txt before building" >&2; exit 1 ;;
esac

tgz="$(mktemp)"
dir="$(mktemp -d)"
trap 'rm -rf "$tgz" "$dir"' EXIT

curl -fsSL -o "$tgz" \
    "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${arch}.tgz"
# Verify the download against the pinned digest before extracting anything.
echo "${sha}  ${tgz}" | sha256sum -c -
tar -xzf "$tgz" -C "$dir"

rel="${dir}/release-${FC_VERSION}-${arch}"
install -m 0755 "${rel}/firecracker-${FC_VERSION}-${arch}" /usr/local/bin/firecracker
install -m 0755 "${rel}/jailer-${FC_VERSION}-${arch}" /usr/local/bin/jailer

# Fail the build if either binary is missing or their versions disagree: forkd
# launches the VMM through the jailer, so the image must carry both at one version.
test -x /usr/local/bin/firecracker
test -x /usr/local/bin/jailer
fc_ver="$(/usr/local/bin/firecracker --version | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' | head -1)"
jl_ver="$(/usr/local/bin/jailer --version | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' | head -1)"
if [ -z "$fc_ver" ] || [ "$fc_ver" != "$jl_ver" ]; then
    echo "install-firecracker: version mismatch firecracker=$fc_ver jailer=$jl_ver" >&2
    exit 1
fi
echo "install-firecracker: installed firecracker and jailer ${fc_ver} (${arch})"
