#!/usr/bin/env bash
#
# mitos-direct.sh -- bare-metal mitos adapter, no Kubernetes.
#
# This adapter measures mitos's create-sandbox -> first-exec on a KVM host
# WITHOUT a cluster, claim path, or kubeconfig. It drives the standalone
# sandbox-server in real mode (issue #257), which forks real Firecracker
# microVMs end to end through the same proven fork engine forkd uses:
# POST /v1/fork restores a snapshot into a live microVM, POST /v1/exec runs a
# command in it over the guest agent vsock path. It is the bare-metal companion
# to adapters/mitos.sh (which needs a cluster + warm SandboxPool).
#
# It keeps the SAME run-comparison.sh contract as adapters/template.sh:
# warm() brings the system to its steady state once; create_exec() forks ONE
# fresh sandbox, execs ONE trivial command, and prints ONE number: the
# create -> first-exec wall-clock MILLISECONDS for that iteration.
#
# "Warm" for mitos-direct: the sandbox-server is already running, the fork
# engine is constructed (KVM validated), and the template snapshot is already
# built (one full microVM boot + Firecracker snapshot, paid once in warm()).
# So the measured number is the WARM fork hot path (snapshot restore + first
# exec round trip), NOT a cold template build. This mirrors mitos.sh, whose
# "warm" is a pre-filled SandboxPool, so the comparison stays apples-to-apples:
# both measure restore-of-a-pre-built-template -> first exec, not first boot.
#
# What it measures per iteration (the create -> first-exec wall clock):
#   t0  = now
#   POST /v1/fork {template, id}      -- restore one fresh microVM
#   POST /v1/exec {sandbox, command}  -- run one trivial command, require exit 0
#   t1  = now
#   sample = (t1 - t0) in ms
# This is the SAME create-sandbox-to-first-exec metric every other adapter
# reports, so run-comparison.sh aggregates them identically.
#
# Required environment (paths to the KVM host's pre-staged artifacts):
#   MITOS_KERNEL    guest kernel (vmlinux) the engine boots templates from.
#   MITOS_DATA_DIR  server data dir. MUST be on a reflink-capable filesystem
#                   (XFS or Btrfs) so each fork's rootfs is copy-on-write, the
#                   designed hot path; on a non-reflink fs the engine falls back
#                   to a full rootfs copy and the number is NOT representative.
# Optional:
#   MITOS_REPO         repo root to build from (default: three levels up).
#   MITOS_AGENT_BIN    prebuilt static guest agent (default: build from repo).
#   MITOS_BUSYBOX      static musl busybox binary (default: download 1.35.0).
#   MITOS_ROOTFS       prebuilt rootfs.ext4 with agent as /init + busybox
#                      (default: build one with mkfs.ext4 + debugfs).
#   MITOS_SERVER_BIN   prebuilt sandbox-server (default: build from repo).
#   MITOS_PORT         server listen port (default: 18432).
#   MITOS_TEMPLATE     template id to pre-build (default: bench).
#
# Sourced by run-comparison.sh; defines warm() and create_exec() only.

: "${MITOS_PORT:=18432}"
: "${MITOS_TEMPLATE:=bench}"
: "${MITOS_BUSYBOX_URL:=https://busybox.net/downloads/binaries/1.35.0-x86_64-linux-musl/busybox}"

# Per-run scratch the adapter owns and cleans up; the server, rootfs, and any
# downloaded busybox live here unless the caller supplied prebuilt paths.
_md_work=""
_md_srv_pid=""
_md_base=""        # http://127.0.0.1:PORT
_md_fork_seq=0     # unique sandbox id counter

_md_cleanup() {
  if [ -n "$_md_srv_pid" ]; then
    kill "$_md_srv_pid" 2>/dev/null || true
    # Give the server a moment to reap its child Firecracker VMs, then force.
    for _ in 1 2 3 4 5; do kill -0 "$_md_srv_pid" 2>/dev/null || break; sleep 0.4; done
    kill -9 "$_md_srv_pid" 2>/dev/null || true
  fi
  # Reap any Firecracker VM this adapter's server started (api-sock under our
  # data dir), so a crash mid-run never leaks a microVM. Scoped to OUR data dir
  # so it never touches an unrelated Firecracker on the host.
  if [ -n "${_md_data_dir:-}" ]; then
    for pid in $(pgrep -f firecracker 2>/dev/null); do
      args="$(tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null)" || continue
      case "$args" in *"$_md_data_dir"*) kill -9 "$pid" 2>/dev/null || true;; esac
    done
  fi
  [ -n "$_md_work" ] && rm -rf "$_md_work" 2>/dev/null || true
}
trap _md_cleanup EXIT

warm() {
  command -v curl >/dev/null 2>&1 || { echo "mitos-direct: curl required" >&2; return 1; }
  command -v jq   >/dev/null 2>&1 || { echo "mitos-direct: jq required" >&2; return 1; }

  if [ -z "${MITOS_KERNEL:-}" ] || [ ! -f "$MITOS_KERNEL" ]; then
    echo "mitos-direct: set MITOS_KERNEL to the guest vmlinux" >&2; return 1
  fi
  if [ -z "${MITOS_DATA_DIR:-}" ]; then
    echo "mitos-direct: set MITOS_DATA_DIR to a reflink-capable (XFS/Btrfs) dir" >&2; return 1
  fi
  mkdir -p "$MITOS_DATA_DIR" || return 1
  _md_data_dir="$MITOS_DATA_DIR"

  # Warn (do not fail) if the data dir lacks reflink: the fork then pays a full
  # rootfs copy and the number is not the designed hot path. Record this in the
  # result so the reader knows whether reflink CoW was in effect.
  local rf_a rf_b
  rf_a="$MITOS_DATA_DIR/.reflink_probe_a"; rf_b="$MITOS_DATA_DIR/.reflink_probe_b"
  if : >"$rf_a" 2>/dev/null && cp --reflink=always "$rf_a" "$rf_b" 2>/dev/null; then
    echo "mitos-direct: reflink CoW available on $MITOS_DATA_DIR (designed hot path)" >&2
  else
    echo "mitos-direct: WARNING reflink CoW NOT available on $MITOS_DATA_DIR; forks will full-copy the rootfs and the number is NOT representative" >&2
  fi
  rm -f "$rf_a" "$rf_b" 2>/dev/null || true

  _md_work="$(mktemp -d -t mitos-direct.XXXXXX)"
  local repo_root agent_bin server_bin rootfs busybox
  repo_root="${MITOS_REPO:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)}"

  # 1) Guest agent (PID 1 at /init): the Rust agent (guest/agent-rs), built as a
  #    static musl binary so it runs as the only userspace in a minimal rootfs.
  #    Reuse a prebuilt one if supplied, else build it the same way the KVM smoke
  #    tools and Dockerfile.forkd do.
  if [ -n "${MITOS_AGENT_BIN:-}" ] && [ -f "$MITOS_AGENT_BIN" ]; then
    agent_bin="$MITOS_AGENT_BIN"
  else
    command -v cargo >/dev/null 2>&1 || { echo "mitos-direct: cargo required to build the agent (or set MITOS_AGENT_BIN)" >&2; return 1; }
    agent_bin="$_md_work/agent"
    ( cd "$repo_root/guest/agent-rs" \
        && rustup target add x86_64-unknown-linux-musl 2>/dev/null \
        && cargo build --release --target x86_64-unknown-linux-musl --features vsock \
        && cp target/x86_64-unknown-linux-musl/release/sandbox-agent "$agent_bin" ) \
      || { echo "mitos-direct: agent build failed" >&2; return 1; }
  fi

  # 2) sandbox-server (the standalone real-mode engine). Reuse a prebuilt one or
  #    build it from the repo.
  if [ -n "${MITOS_SERVER_BIN:-}" ] && [ -f "$MITOS_SERVER_BIN" ]; then
    server_bin="$MITOS_SERVER_BIN"
  else
    command -v go >/dev/null 2>&1 || { echo "mitos-direct: go required to build sandbox-server (or set MITOS_SERVER_BIN)" >&2; return 1; }
    server_bin="$_md_work/sandbox-server"
    ( cd "$repo_root" && go build -o "$server_bin" ./cmd/sandbox-server/ ) \
      || { echo "mitos-direct: sandbox-server build failed" >&2; return 1; }
  fi

  # 3) busybox: a static musl build supplies /bin/sh and the trivial command.
  if [ -n "${MITOS_BUSYBOX:-}" ] && [ -f "$MITOS_BUSYBOX" ]; then
    busybox="$MITOS_BUSYBOX"
  else
    busybox="$_md_work/busybox"
    curl -fsSL -o "$busybox" "$MITOS_BUSYBOX_URL" \
      || { echo "mitos-direct: busybox download failed from $MITOS_BUSYBOX_URL (set MITOS_BUSYBOX)" >&2; return 1; }
    chmod +x "$busybox"
  fi

  # 4) rootfs.ext4: the engine passes a file-path rootfs straight through and
  #    does NOT inject the agent for file-path templates (only for OCI image
  #    builds), so the rootfs we hand it must already carry the agent at /init
  #    plus busybox. Reuse a prebuilt rootfs or assemble one with mkfs.ext4 +
  #    debugfs (no loopback mount, so no root-mount requirement beyond debugfs).
  if [ -n "${MITOS_ROOTFS:-}" ] && [ -f "$MITOS_ROOTFS" ]; then
    rootfs="$MITOS_ROOTFS"
  else
    command -v mkfs.ext4 >/dev/null 2>&1 || { echo "mitos-direct: mkfs.ext4 required (or set MITOS_ROOTFS)" >&2; return 1; }
    command -v debugfs   >/dev/null 2>&1 || { echo "mitos-direct: debugfs required (or set MITOS_ROOTFS)" >&2; return 1; }
    rootfs="$_md_work/rootfs.ext4"
    # 192 MiB is ample for a static agent + busybox.
    truncate -s 192M "$rootfs" || return 1
    mkfs.ext4 -q -F "$rootfs" >/dev/null 2>&1 || { echo "mitos-direct: mkfs.ext4 failed" >&2; return 1; }
    # Lay out /init (agent), /bin/busybox, and busybox symlinks for the shell
    # utilities the exec command needs. debugfs writes into the image offline.
    debugfs -w -R "write $agent_bin init" "$rootfs" >/dev/null 2>&1 || { echo "mitos-direct: write /init failed" >&2; return 1; }
    debugfs -w -R "set_inode_field init mode 0100755" "$rootfs" >/dev/null 2>&1 || true
    for d in bin dev proc sys tmp etc run; do
      debugfs -w -R "mkdir /$d" "$rootfs" >/dev/null 2>&1 || true
    done
    debugfs -w -R "write $busybox bin/busybox" "$rootfs" >/dev/null 2>&1 || { echo "mitos-direct: write busybox failed" >&2; return 1; }
    debugfs -w -R "set_inode_field bin/busybox mode 0100755" "$rootfs" >/dev/null 2>&1 || true
    for applet in sh echo true cat ls head tr env; do
      debugfs -w -R "symlink /bin/$applet /bin/busybox" "$rootfs" >/dev/null 2>&1 || true
    done
  fi

  # 5) Start the server ONCE and wait for health.
  _md_base="http://127.0.0.1:${MITOS_PORT}"
  "$server_bin" \
    --kernel "$MITOS_KERNEL" \
    --rootfs "$rootfs" \
    --agent-bin "$agent_bin" \
    --data-dir "$MITOS_DATA_DIR" \
    --addr ":${MITOS_PORT}" >"$_md_work/server.log" 2>&1 &
  _md_srv_pid=$!

  local ok=""
  for _ in $(seq 1 100); do
    if curl -fsS "$_md_base/v1/health" >/dev/null 2>&1; then ok=1; break; fi
    if ! kill -0 "$_md_srv_pid" 2>/dev/null; then
      echo "mitos-direct: server exited during startup; log:" >&2
      tail -20 "$_md_work/server.log" >&2 || true
      return 1
    fi
    sleep 0.2
  done
  [ -n "$ok" ] || { echo "mitos-direct: server health never came up; log:" >&2; tail -20 "$_md_work/server.log" >&2; return 1; }

  # 6) Pre-build the template snapshot once (one full microVM boot + snapshot).
  #    This is the cost warm() absorbs so create_exec() measures the warm fork
  #    hot path, not first boot.
  if ! curl -fsS -X POST "$_md_base/v1/templates" -d "{\"id\":\"$MITOS_TEMPLATE\"}" >/dev/null 2>&1; then
    echo "mitos-direct: template build failed; server log:" >&2
    tail -30 "$_md_work/server.log" >&2 || true
    return 1
  fi
  echo "mitos-direct warmed: server up on $_md_base, template '$MITOS_TEMPLATE' built" >&2
}

create_exec() {
  _md_fork_seq=$((_md_fork_seq + 1))
  local sid t0 t1 fork_http exec_out exit_code
  sid="md-$$-$_md_fork_seq"

  # Start the create -> first-exec wall clock at the fork request, stop it after
  # the first successful exec returns. This is the create-sandbox-to-first-exec
  # metric, identical to every other adapter.
  t0=$(date +%s%N)

  fork_http="$(curl -sS -o /dev/null -w '%{http_code}' \
    -X POST "$_md_base/v1/fork" -d "{\"template\":\"$MITOS_TEMPLATE\",\"id\":\"$sid\"}" 2>/dev/null)" || return 1
  if [ "$fork_http" != "200" ]; then
    echo "mitos-direct: fork returned HTTP $fork_http for $sid" >&2
    return 1
  fi

  exec_out="$(curl -sS -X POST "$_md_base/v1/exec" \
    -d "{\"sandbox\":\"$sid\",\"command\":\"true\"}" 2>/dev/null)" || return 1
  exit_code="$(printf '%s' "$exec_out" | jq -r '.exit_code // empty' 2>/dev/null)"
  if [ "$exit_code" != "0" ]; then
    echo "mitos-direct: exec did not return exit 0 for $sid: $exec_out" >&2
    return 1
  fi

  t1=$(date +%s%N)

  # Tear the sandbox down so its microVM does not accumulate across iterations.
  # Best effort and OFF the measured window; a failure here does not poison the
  # sample, but a leaked VM would skew later forks, so we try DELETE then the
  # terminate route the server exposes.
  curl -sS -X DELETE "$_md_base/v1/sandboxes/$sid" >/dev/null 2>&1 \
    || curl -sS -X POST "$_md_base/v1/terminate" -d "{\"sandbox\":\"$sid\"}" >/dev/null 2>&1 \
    || true

  # Print ONE number: elapsed milliseconds, integer.
  awk -v a="$t0" -v b="$t1" 'BEGIN { printf "%d\n", (b - a) / 1000000 }'
}
