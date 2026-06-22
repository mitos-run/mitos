# Matched-hardware competitor comparison (2026-06-22)

Issue #15 item 3 (the competitor comparison table). This run produces the mitos
column as OUR measurement on bare-metal KVM via the comparison harness, and
records the competitor attempt honestly: deployed and brought up, but the
create -> first-exec measurement blocked at headless authentication, with NO
fabricated number.

Per CLAUDE.md operating principle 1 (no unverified claims): every number below
is reproducible from the adapters in `bench/competitors/adapters/` on the same
hardware. Where there is no measurement, the cell says "blocked: <reason>", not
a number.

## Hardware (both boxes, Hetzner bare metal)

Both boxes are identical Hetzner dedicated servers:

| | value (measured on the box) |
| --- | --- |
| CPU | Intel(R) Core(TM) i7-6700 @ 3.40GHz |
| topology | 1 socket, 4 cores, 2 threads/core = 8 logical CPUs (`nproc` = 8) |
| RAM | 62 GiB total |
| host kernel | 6.1.0-49-amd64 (Debian 12) |
| KVM | `/dev/kvm` present, hardware virtualization in use |

Note: the campaign brief described these as "8c/16t"; the actual silicon is
4c/8t (i7-6700). Recorded as measured, not as briefed.

Role split (so the two systems never contend for the same host):

- box1 (mitos-bench1): mitos-direct measurement.
- box2 (mitos-bench2): Daytona OSS deployment attempt.

## mitos-direct (box1) -- REAL measurement

Method: the standalone `sandbox-server` in real mode (issue #257) forks real
Firecracker microVMs end to end through the same fork engine forkd uses. The
adapter `bench/competitors/adapters/mitos-direct.sh` drives the SAME
create-sandbox -> first-exec metric every adapter in this directory reports, via
`bench/competitors/run-comparison.sh`.

Per iteration the adapter times, as wall clock:

1. `POST /v1/fork {template, id}` -- restore one fresh microVM from the warmed
   template snapshot.
2. `POST /v1/exec {sandbox, command:"true"}` -- run one trivial command, require
   exit 0.

"Warm" state (absorbed once in `warm()`, NOT in the measured window): the server
is running, the fork engine is constructed (KVM validated), and the template
snapshot is pre-built (one full microVM boot + Firecracker full snapshot). So
the measured number is the WARM fork hot path (snapshot restore + first exec
round trip), the same warm-restore semantics as the cluster `mitos.sh` adapter's
pre-filled SandboxPool, not a cold template build.

### Stack under test

| | value |
| --- | --- |
| repo commit | this branch (issue #257 `--agent-bin` sandbox-server), synced to box1 |
| firecracker | v1.15.0 (`/usr/local/bin/firecracker`) |
| guest kernel | 6.1.155+ (`/root/mitos-test/vmlinux`) |
| guest config | 1 vCPU, 512 MiB (sandbox-server `DefaultVMConfig`) |
| rootfs | 192 MiB ext4, guest agent as `/init` + static musl busybox 1.35.0 (assembled by the adapter with `mkfs.ext4` + `debugfs`) |
| go | go1.26.4 |
| data dir | `/data` (XFS) -- reflink CoW confirmed available, so each fork's rootfs is copy-on-write (the designed hot path), NOT a full copy |

reflink matters: `/data` is XFS with reflink (the adapter probes and confirms
this in `warm()`); `/tmp` and `/root` on this box are ext4 WITHOUT reflink, where
the engine falls back to a full rootfs copy and the number would not be
representative. The measurement was taken on `/data`.

### Result (N=20 measured, 3 warmup discarded)

```
create -> first-exec (ms), N=20:
  min  164
  P50  180
  P90  202
  P99  204
  max  204
```

Raw samples in iteration order:

```
180 174 177 168 173 181 204 164 170 184 185 202 185 185 198 179 187 202 177 166
```

Raw samples sorted:

```
164 166 168 170 173 174 177 177 179 180 181 184 185 185 185 187 198 202 202 204
```

This is mitos's matched-method create -> first-exec on bare-metal KVM: roughly a
180 ms P50 warm fork-to-exec, no Kubernetes, no cluster, no pool. The full
harness output (warm log, per-iter lines, percentiles) is reproducible by
re-running the adapter; see "How to reproduce" below.

## Daytona OSS (box2) -- DEPLOYED, measurement BLOCKED (no number fabricated)

Daytona OSS (github.com/daytonaio/daytona, commit 4ee2c63) was chosen as the
most self-hostable competitor and was deployed on box2 via the project's own
`docker compose`. The stack came up cleanly:

```
docker compose -f docker/docker-compose.yaml ps  ->  14/14 services up
api: Up (healthy)        proxy: Up (healthy)      runner: Up (healthy)
dex: Up (healthy)        db: Up                   redis: Up
registry: Up             registry-ui: Up          minio: Up
ssh-gateway: Up          maildev: Up (healthy)    pgadmin: Up
jaeger: Up               otel-collector: Up
```

The blocker is HEADLESS AUTHENTICATION, not the deploy. The create and exec
endpoints exist and were identified from the running OSS swagger (`/api-json`):

- create: `POST /api/sandbox`
- exec:   `POST /api/toolbox/{id}/toolbox/process/execute`
- delete: `DELETE /api/sandbox/{id}`

Both require a user-scoped bearer token / API key. Verbatim, from the running
stack:

```
POST /api/sandbox  (no auth) -> HTTP 401 {"statusCode":401,"error":"Unauthorized","message":"Invalid credentials"}
POST /api/api-keys (no auth) -> HTTP 401 {"statusCode":401,"error":"Unauthorized","message":"Invalid credentials"}
```

Minting that API key requires completing the Dex OIDC login, and the default Dex
ships only browser flows:

```
grant_types_supported = ["authorization_code","refresh_token","urn:ietf:params:oauth:grant-type:device_code","urn:ietf:params:oauth:grant-type:token-exchange"]
```

There is NO resource-owner password grant. Three distinct headless attempts were
made and all terminate in a Dex web login form (the default
`dev@daytona.io / password` user) that a curl-only harness cannot drive cleanly:

1. SPA authorization-code flow: the dashboard's exact registered redirect_uri is
   required; a guessed redirect_uri returns 400 at `/dex/auth/local`.
2. Device-code flow: `POST /dex/device/code` succeeds and returns a user_code,
   but `/dex/device/auth/verify_code` redirects into the same browser login
   form, and following it via curl returns 400 (missing browser session state).
3. Direct local-connector POST: 400 without the in-browser auth-request state.

Conclusion: a real Daytona create -> first-exec number on this hardware needs a
human to log in once via the dashboard (`http://localhost:3000`,
`dev@daytona.io / password`), mint an API key, and provision a snapshot image
into the internal registry. The adapter `bench/competitors/adapters/daytona.sh`
is wired with the REAL create + exec calls and runs end to end once a
`DAYTONA_API_KEY` is supplied; without it the adapter exits non-zero so the
harness can never emit a fabricated Daytona number.

Daytona result: **blocked: OSS sandbox API requires a Dex-OIDC-minted user API
key; Dex default config exposes only browser authorization-code / device-code
flows (no password grant), so no headless token in the timebox.** No number.

## What is NOT yet measured, and why

- **Daytona create -> first-exec on matched hardware**: blocked as above
  (headless auth). Unblock by minting an API key via the dashboard and re-running
  `daytona.sh` with `DAYTONA_API_KEY` set; the snapshot image must also be ready
  in the internal registry first.
- **E2B (self-hosted)**: NOT attempted. E2B self-hosted is a heavy multi-service
  infra stack (Nomad/Consul cluster, custom orchestrator, KVM firecracker
  runners) and is out of this run's timebox by design. Left as a reproducer step
  in `adapters/e2b.sh`; any E2B figure remains vendor-published until measured
  here.
- **Modal / other hosted-only services**: not self-hostable, so any number comes
  from the vendor's hosted hardware, not this reference node; recorded as
  vendor-published, not our measurement (see the fan-out section of the
  competitors README).
- **mitos cluster claim path (`adapters/mitos.sh`)**: not run here; it needs a
  kubeconfig + warm SandboxPool we do not have on these boxes. `mitos-direct.sh`
  is the bare-metal, no-cluster companion measured above.

## Comparison table (matched method, matched hardware)

| system | create -> first-exec | source |
| --- | --- | --- |
| mitos (mitos-direct, bare-metal KVM) | min 164 / P50 180 / P90 202 / P99 204 / max 204 ms (N=20) | OUR measurement, this run (`adapters/mitos-direct.sh`) |
| Daytona OSS (self-hosted) | blocked: headless auth (Dex OIDC, no password grant) | deployed on box2 this run; no number, see above |
| E2B (self-hosted) | not measured (out of timebox; heavy multi-service stack) | vendor-published until `adapters/e2b.sh` is run here |

## How to reproduce

### mitos-direct (box1)

Prereqs on the KVM host: `firecracker`, a guest `vmlinux`, `go`, `mkfs.ext4`,
`debugfs`, `curl`, `jq`, and a reflink-capable data dir (XFS or Btrfs).

```bash
export MITOS_KERNEL=/path/to/vmlinux
export MITOS_DATA_DIR=/data/mybench       # MUST be XFS or Btrfs (reflink)
cd <repo>
bench/competitors/run-comparison.sh bench/competitors/adapters/mitos-direct.sh 20 3
```

The adapter builds `sandbox-server` and the guest agent from the repo, downloads
static busybox 1.35.0, assembles the rootfs, starts the server once, pre-builds
the template, and then measures 20 forks. Optional `MITOS_AGENT_BIN`,
`MITOS_SERVER_BIN`, `MITOS_BUSYBOX`, `MITOS_ROOTFS` reuse prebuilt artifacts.

### Daytona OSS (box2)

```bash
# install docker (https://get.docker.com) + git, then:
git clone --depth 1 https://github.com/daytonaio/daytona.git
cd daytona && docker compose -f docker/docker-compose.yaml up -d
# open http://localhost:3000, log in (dev@daytona.io / password), mint an API
# key, ensure a snapshot image is ready, then:
export DAYTONA_API_KEY=<minted key>
cd <repo>
bench/competitors/run-comparison.sh bench/competitors/adapters/daytona.sh 20 3
```
