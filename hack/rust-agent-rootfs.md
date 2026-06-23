# Rootfs bake steps for the Rust-agent template

This document describes how to produce a second Firecracker template whose only
difference from the Go-agent baseline is the guest agent binary. `cmd/bench`
can then fork both templates under identical conditions for an apples-to-apples
comparison (issue #310).

All steps that touch KVM or `forkd CreateTemplate` must run on a Linux host with
`/dev/kvm`. The reference environment is the Hetzner KVM box used for bench
runs (#15).

---

## 1. Build the static Rust binary

Run the provided script from the repo root on a Linux x86-64 host:

```sh
bash hack/build-rust-agent.sh
```

The script adds the `x86_64-unknown-linux-musl` Rust target if needed, then
builds a statically linked release binary. The output path is:

```
guest/agent-rs/target/x86_64-unknown-linux-musl/release/sandbox-agent
```

Verify the binary is statically linked before continuing:

```sh
file guest/agent-rs/target/x86_64-unknown-linux-musl/release/sandbox-agent
# expect: ELF 64-bit LSB executable, x86-64, statically linked
```

---

## 2. Clone the Go-agent template rootfs

The engine loads templates from this layout (from `bench/README.md`):

```
<data-dir>/templates/<id>/snapshot/mem
<data-dir>/templates/<id>/snapshot/vmstate
<data-dir>/templates/<id>/rootfs.ext4
<data-dir>/templates/<id>/verified
```

The rootfs must contain the guest agent as `/init`. The Go template already has
the Go agent at `/init`; the Rust template replaces only that binary and keeps
the kernel and every other rootfs byte identical.

Choose a template id for the Rust variant, for example `<base>-rustagent`.
Clone the Go template directory:

```sh
BASE_ID=<your-go-template-id>
RUST_ID="${BASE_ID}-rustagent"
DATA_DIR=<data-dir>

cp -r "${DATA_DIR}/templates/${BASE_ID}" "${DATA_DIR}/templates/${RUST_ID}"
```

Do not touch the snapshot files (`snapshot/mem`, `snapshot/vmstate`) at this
point; they will be replaced in step 3 when forkd re-snapshots the VM.

---

## 3. Replace /init in the cloned rootfs

Mount the cloned `rootfs.ext4` loopback, replace `/init`, and unmount. These
commands require `root` or `sudo`.

```sh
ROOTFS="${DATA_DIR}/templates/${RUST_ID}/rootfs.ext4"
MOUNT_POINT=$(mktemp -d)
RUST_BIN=guest/agent-rs/target/x86_64-unknown-linux-musl/release/sandbox-agent

sudo mount -o loop "${ROOTFS}" "${MOUNT_POINT}"

sudo cp "${RUST_BIN}" "${MOUNT_POINT}/init"
sudo chmod 0755 "${MOUNT_POINT}/init"

sudo umount "${MOUNT_POINT}"
rmdir "${MOUNT_POINT}"
```

The binary lands at `/init` inside the ext4 image, the same path the Go agent
occupies. No other file in the rootfs changes.

---

## 4. Re-snapshot through forkd CreateTemplate

The engine requires the snapshot to have been created with a relative vsock
`uds_path` (`vsock.sock`) so that per-fork working directories do not collide
on the host socket. The cleanest way to satisfy this and produce the `verified`
marker is to boot the VM and snapshot it through forkd's `CreateTemplate` flow,
which content-addresses the result into the CAS store and writes the `verified`
file automatically.

Call `CreateTemplate` via the forkd gRPC API (port 9090 on the KVM host),
passing the Rust template id and the path to the modified rootfs. The exact
invocation depends on the gRPC client you use (`grpcurl` or the internal
`forkd` CLI):

```sh
# example using grpcurl; adjust the proto import path and field names to match
# your local forkd build
grpcurl -plaintext \
  -d "{\"template_id\": \"${RUST_ID}\", \"rootfs_path\": \"${ROOTFS}\"}" \
  localhost:9090 \
  forkd.ForkService/CreateTemplate
```

`CreateTemplate` boots the VM with the Rust `/init`, takes a Firecracker
snapshot, content-addresses the snapshot files, copies them into
`<data-dir>/templates/<id>/snapshot/`, and writes the `verified` marker. The
resulting layout is the same as the Go template:

```
<data-dir>/templates/<id>/snapshot/mem
<data-dir>/templates/<id>/snapshot/vmstate
<data-dir>/templates/<id>/rootfs.ext4
<data-dir>/templates/<id>/verified
```

If you instead lay out the snapshot files by hand (the same method the CI bench
phase uses), the engine will refuse to fork an unverified snapshot. In that case
touch the marker manually only for local testing:

```sh
touch "${DATA_DIR}/templates/${RUST_ID}/verified"
```

---

## 5. Record the template id

Write down the resulting template id (e.g. `<base>-rustagent`) alongside the
run metadata. An example record for `bench/results/`:

```
template-go:    <base>
template-rust:  <base>-rustagent
kernel:         <data-dir>/vmlinux   (byte-identical for both)
rootfs-base:    byte-identical ext4, same kernel, same shell, only /init differs
agent-go:       guest/agent/ (Go, dynamic build inside rootfs)
agent-rust:     guest/agent-rs/target/x86_64-unknown-linux-musl/release/sandbox-agent
firecracker:    v1.15.0
host:           <CPU, kernel version, Hetzner node label>
date:           <ISO-8601 date>
```

The only variable between the two templates is the agent binary at `/init`. The
kernel, rootfs filesystem, and Firecracker version are otherwise identical, so
latency differences are attributable solely to the agent implementation.

---

## 6. Run cmd/bench against both templates

`cmd/bench` is unchanged; point it at each template id in turn.

```sh
go build -o /tmp/bench ./cmd/bench/

# Go agent baseline
/tmp/bench \
  --mode fork-exec \
  --template "${BASE_ID}" \
  --data-dir "${DATA_DIR}" \
  --firecracker /usr/local/bin/firecracker \
  --kernel "${DATA_DIR}/vmlinux" \
  --iterations 100 --warmup 10 \
  --summary --json "bench-go.json"

# Rust agent
/tmp/bench \
  --mode fork-exec \
  --template "${RUST_ID}" \
  --data-dir "${DATA_DIR}" \
  --firecracker /usr/local/bin/firecracker \
  --kernel "${DATA_DIR}/vmlinux" \
  --iterations 100 --warmup 10 \
  --summary --json "bench-rust.json"
```

Archive both JSON files and the record from step 5 together. Do not publish
numbers before the runs complete; see `bench/README.md` and CLAUDE.md operating
principle 1 (no unverified claims).
