#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../guest/agent-rs"
rustup target add x86_64-unknown-linux-musl >/dev/null 2>&1 || true
cargo build --release --target x86_64-unknown-linux-musl
BIN=target/x86_64-unknown-linux-musl/release/sandbox-agent
file "$BIN"                       # expect: statically linked
printf 'rust-agent size (bytes): %s\n' "$(stat -c%s "$BIN" 2>/dev/null || stat -f%z "$BIN")"
