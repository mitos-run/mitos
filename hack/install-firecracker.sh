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
# Requires curl and ca-certificates to be present already (the caller installs them).
set -euo pipefail

FC_VERSION="${FC_VERSION:-v1.15.0}"

case "$(uname -m)" in
    x86_64|amd64) arch="x86_64" ;;
    aarch64|arm64) arch="aarch64" ;;
    *) echo "install-firecracker: unsupported arch $(uname -m)" >&2; exit 1 ;;
esac

tgz="$(mktemp)"
dir="$(mktemp -d)"
trap 'rm -rf "$tgz" "$dir"' EXIT

curl -fsSL -o "$tgz" \
    "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${arch}.tgz"
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
