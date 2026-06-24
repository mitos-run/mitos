#!/usr/bin/env bash
# hack/build-rust-agent.sh: build the Rust guest agent as a static musl binary.
#
# Usage: bash hack/build-rust-agent.sh [--no-size-gate]
#
# Builds the sandbox-agent crate (guest/agent-rs/) for x86_64-unknown-linux-musl
# with the vsock feature enabled (required for production). The release profile
# strips the binary (strip = true in Cargo.toml). Exits non-zero if the size
# gate fails or the build fails.
#
# CI gate commands (run these in CI for the Rust agent):
#   cargo clippy --features vsock --all-targets -- -D warnings
#   cargo fmt --check
#   cargo build --release --target x86_64-unknown-linux-musl --features vsock
#   cargo test --features vsock
#   bash hack/build-rust-agent.sh
#
# Size gate: set to 110% of the first measured production binary (task 5.3).
# Measured on box2 (x86_64 musl, tonic+tokio, vsock feature):
# FIRST_MEASURED_BYTES is recorded below; update to 110% after this PR merges.
# The gate is intentionally generous until the binary stabilizes.
# Principle 1: no unverified claims; the number below comes from the first
# real musl build, not a projection.

set -euo pipefail

CRATE_DIR="$(cd "$(dirname "$0")/../guest/agent-rs" && pwd)"
TARGET="x86_64-unknown-linux-musl"
BIN="$CRATE_DIR/target/$TARGET/release/sandbox-agent"

# Size gate in bytes. Measured on box2 (x86_64 musl, release, vsock, task 5.3):
# 2,500,448 bytes (2.38 MiB) -- tonic 0.13 + tokio + protobuf + vsock, stripped.
# Gate set to 110% of the measured size to allow for minor dependency updates.
# Update this value if a large dependency is intentionally added.
SIZE_GATE_BYTES=2750493

NO_GATE=0
for arg in "$@"; do
  if [ "$arg" = "--no-size-gate" ]; then
    NO_GATE=1
  fi
done

echo "==> Building Rust guest agent (musl static, vsock feature)"
echo "    crate: $CRATE_DIR"
echo "    target: $TARGET"

(
  cd "$CRATE_DIR"
  cargo build --release --target "$TARGET" --features vsock
)

if [ ! -f "$BIN" ]; then
  echo "FAIL: binary not found at $BIN" >&2
  exit 1
fi

# Verify the binary is statically linked.
FILE_OUT=$(file "$BIN")
echo ""
echo "==> Binary info:"
echo "    $FILE_OUT"
if ! echo "$FILE_OUT" | grep -q "statically linked"; then
  echo "FAIL: binary is not statically linked" >&2
  exit 1
fi

ACTUAL_BYTES=$(stat -c%s "$BIN" 2>/dev/null || stat -f%z "$BIN")
echo ""
printf '==> rust-agent size check: %d bytes (gate: <= %d bytes)\n' \
  "$ACTUAL_BYTES" "$SIZE_GATE_BYTES"

if [ "$NO_GATE" = "0" ]; then
  if [ "$ACTUAL_BYTES" -gt "$SIZE_GATE_BYTES" ]; then
    echo "FAIL: binary size $ACTUAL_BYTES exceeds gate $SIZE_GATE_BYTES" >&2
    exit 1
  fi
  echo "==> Size gate: PASS"
fi

echo ""
echo "==> Build OK: $BIN"
