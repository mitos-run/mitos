#!/usr/bin/env bash
set -euo pipefail

# Builds a minimal rootfs ext4 image with the guest agent baked in.
# Must run on Linux as root (needs mount, chroot, debootstrap).
#
# Usage:
#   ./guest/rootfs/build.sh [output_path] [size_mb]
#
# Agent implementation selector:
#   AGENT_IMPL=go   (default, unset means go) - builds the Go guest agent
#                   (guest/agent) as /init. This is the production default;
#                   the Go agent serves BOTH the legacy JSON protocol on
#                   vsock port 52 (AgentPort) and the gRPC protocol on
#                   vsock port 53 (AgentGRPCPort).
#   AGENT_IMPL=rust - builds the Rust guest agent (guest/agent-rs) as /init.
#                   The Rust agent serves ONLY gRPC on vsock port 53.
#                   IMPORTANT: baking the Rust agent as /init makes the gRPC
#                   surface work (bench, conformance), but the production
#                   fork/exec/file host-side callers still speak the legacy
#                   JSON protocol on port 52. A rootfs baked with AGENT_IMPL=rust
#                   will NOT function on the production data path until the
#                   JSON->gRPC host-caller migration (SP1.5) lands. Use only for
#                   gRPC bench and conformance testing. See hack/rust-agent-cutover.md.
#
# Re-baking with AGENT_IMPL=go (or with AGENT_IMPL unset) restores the Go agent.
# The selector is reversible: only /init changes; the rest of the rootfs is identical.
#
# Produces: rootfs.ext4 with:
#   /init              -> guest agent binary (PID 1)
#   /bin/sh            -> busybox or bash
#   /usr/bin/python3   -> Python 3 + ipykernel/jupyter_client (FULL_ROOTFS=1)
#   /opt/mitos/kernel_driver.py -> run_code kernel driver (FULL_ROOTFS=1)
#   /workspace/        -> agent working directory

OUTPUT="${1:-/tmp/mitos-rootfs.ext4}"
# The full rootfs bakes ipykernel + matplotlib + pandas for run_code, which need
# headroom beyond the minimal image; the busybox path is much smaller.
SIZE_MB="${2:-1024}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
WORK_DIR=$(mktemp -d)
MOUNT_DIR="${WORK_DIR}/mnt"

# AGENT_IMPL: "go" (default) or "rust" (opt-in gRPC-only build).
AGENT_IMPL="${AGENT_IMPL:-go}"

cleanup() {
    umount "$MOUNT_DIR" 2>/dev/null || true
    rm -rf "$WORK_DIR"
}
trap cleanup EXIT

if [ "$AGENT_IMPL" = "rust" ]; then
    echo "==> Building Rust guest agent (static musl binary, gRPC-only, vsock port 53)"
    echo "    NOTE: this agent serves ONLY gRPC (port 53). The production host-side"
    echo "    callers still speak the legacy JSON protocol (port 52). This rootfs is"
    echo "    for gRPC bench and conformance only. See hack/rust-agent-cutover.md."
    cd "$PROJECT_ROOT/guest/agent-rs"
    cargo build --release --target x86_64-unknown-linux-musl --features vsock
    cp "$PROJECT_ROOT/guest/agent-rs/target/x86_64-unknown-linux-musl/release/sandbox-agent" "${WORK_DIR}/agent"
else
    echo "==> Building Go guest agent (static binary, JSON+gRPC, ports 52+53)"
    cd "$PROJECT_ROOT"
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o "${WORK_DIR}/agent" ./guest/agent/
fi

echo "==> Creating ext4 image (${SIZE_MB}MB)"
dd if=/dev/zero of="$OUTPUT" bs=1M count="$SIZE_MB" status=none
mkfs.ext4 -q -F "$OUTPUT"

echo "==> Mounting and populating rootfs"
mkdir -p "$MOUNT_DIR"
mount -o loop "$OUTPUT" "$MOUNT_DIR"

# Create directory structure
mkdir -p "$MOUNT_DIR"/{bin,sbin,usr/bin,usr/lib,lib,lib64,dev,proc,sys,tmp,run,etc,workspace,var/log}

# Install the guest agent as /init
cp "${WORK_DIR}/agent" "$MOUNT_DIR/init"
chmod +x "$MOUNT_DIR/init"

# Check if we should use debootstrap for a full Ubuntu rootfs or minimal busybox
if command -v debootstrap &>/dev/null && [ "${FULL_ROOTFS:-0}" = "1" ]; then
    echo "==> Installing Ubuntu minimal via debootstrap"
    debootstrap --variant=minbase noble "$MOUNT_DIR" http://archive.ubuntu.com/ubuntu

    # Install Python
    chroot "$MOUNT_DIR" apt-get update -qq
    chroot "$MOUNT_DIR" apt-get install -y --no-install-recommends \
        python3 python3-pip python3-venv ca-certificates curl

    # Code-interpreter kernel: ipykernel + jupyter_client give the run_code
    # surface a stateful Python kernel with rich display (matplotlib png, pandas
    # html) and structured errors. matplotlib_inline routes figures to png
    # display_data. Installed only in the full rootfs; the minimal busybox image
    # has no Python and run_code returns KernelUnavailable there.
    chroot "$MOUNT_DIR" python3 -m pip install --no-cache-dir \
        ipykernel jupyter_client matplotlib matplotlib_inline pandas
    chroot "$MOUNT_DIR" python3 -m ipykernel install --name python3 --sys-prefix

    # Install the in-guest kernel driver the agent spawns for run_code.
    mkdir -p "$MOUNT_DIR/opt/mitos"
    cp "$PROJECT_ROOT/guest/rootfs/kernel_driver.py" "$MOUNT_DIR/opt/mitos/kernel_driver.py"
    chmod +x "$MOUNT_DIR/opt/mitos/kernel_driver.py"

    chroot "$MOUNT_DIR" apt-get clean
    chroot "$MOUNT_DIR" rm -rf /var/lib/apt/lists/*

    # Symlink init
    rm -f "$MOUNT_DIR/sbin/init"
    ln -s /init "$MOUNT_DIR/sbin/init"
else
    echo "==> Installing busybox (minimal rootfs)"
    # Download static busybox
    BUSYBOX_URL="https://busybox.net/downloads/binaries/1.35.0-x86_64-linux-musl/busybox"
    if command -v curl &>/dev/null; then
        curl -fsSL -o "$MOUNT_DIR/bin/busybox" "$BUSYBOX_URL"
    elif command -v wget &>/dev/null; then
        wget -q -O "$MOUNT_DIR/bin/busybox" "$BUSYBOX_URL"
    fi
    chmod +x "$MOUNT_DIR/bin/busybox"

    # Create symlinks for common commands
    for cmd in sh ash ls cat echo mkdir rm cp mv chmod chown ln env wc head tail grep sed awk sort uniq tr tee; do
        ln -sf /bin/busybox "$MOUNT_DIR/bin/$cmd"
    done
    for cmd in python3 pip3; do
        # Stub: real Python needs debootstrap or a pre-built rootfs
        cat > "$MOUNT_DIR/usr/bin/$cmd" << 'PYSTUB'
#!/bin/sh
echo "Python not available in minimal rootfs. Use FULL_ROOTFS=1 to build with Python."
exit 1
PYSTUB
        chmod +x "$MOUNT_DIR/usr/bin/$cmd"
    done
fi

# Basic config files
echo "sandbox" > "$MOUNT_DIR/etc/hostname"
echo "root:x:0:0:root:/root:/bin/sh" > "$MOUNT_DIR/etc/passwd"
echo "root:x:0:" > "$MOUNT_DIR/etc/group"
echo "nameserver 8.8.8.8" > "$MOUNT_DIR/etc/resolv.conf"

# Create /etc/os-release
cat > "$MOUNT_DIR/etc/os-release" << 'EOF'
NAME="sandbox"
ID=sandbox
VERSION="1.0"
PRETTY_NAME="sandbox rootfs"
EOF

echo "==> Unmounting"
umount "$MOUNT_DIR"

# Report
SIZE=$(du -sh "$OUTPUT" | cut -f1)
echo ""
echo "================================"
echo "  Rootfs built: $OUTPUT ($SIZE)"
echo "================================"
