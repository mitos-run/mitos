# VMM seccomp: enforcement and the custom-profile evaluation

Issue: #353. Related: `docs/threat-model.md` section 1 (the "Seccomp on the VMM
process" row), `internal/firecracker/seccomp.go`, `internal/firecracker/client.go`,
`.github/workflows/kvm-test.yaml` (the "Seccomp filter enforced on the Firecracker
VMM" step).

## Why this matters

The Firecracker VMM is the host-facing edge of the isolation boundary. KVM is the
primary wall; the VMM's seccomp BPF filter is the second wall behind it. A microVM
escape that lands in an unfiltered VMM process inherits the VMM's full host
syscall surface, so the difference between "Firecracker probably filters" and "we
assert the filter holds" is the difference the threat model is held to.

## What Firecracker provides

Firecracker installs a production seccomp BPF filter on the VMM threads when the
microVM STARTS (the vCPUs are launched, on `InstanceStart` or a snapshot resume),
UNLESS the operator passes `--no-seccomp`. The filter is NOT installed merely by
starting the API server: a Firecracker that has only opened its API socket and is
idle (no VM configured) reports seccomp mode 0 on its main thread, which is why
the CI proof boots a microVM before checking. The filter is the "advanced" level:
an allowlist scoped to the syscalls (and, for some, the argument values) the VMM
actually needs on its supported paths. It is maintained in the Firecracker tree
and tested against each Firecracker release.

## Enforcement in Mitos (what ships)

1. **Never disabled.** No Mitos launch path passes `--no-seccomp`. Both the
   direct-exec path (`StartVM`) and the jailer path (`startJailedVM`) call
   `assertSeccompEnforced` on the final argv before exec, which FAILS CLOSED if
   `--no-seccomp` (or `--no-seccomp=...`) is ever present. This turns "we do not
   pass it" into a checked invariant that a future flag or refactor cannot
   silently break. Unit-tested in `internal/firecracker/seccomp_test.go`.

2. **Proven on KVM.** The `kvm-test.yaml` step BOOTS a real microVM on a KVM
   runner (Firecracker installs its filters at vCPU start, not at API-server
   start) and asserts a VMM thread reports `Seccomp: 2` (SECCOMP_MODE_FILTER) in
   `/proc/<pid>/task/*/status`, so a disallowed syscall is blocked by the
   installed filter. A `--no-seccomp` boot is the negative control: every thread
   stays mode 0, proving the check distinguishes enforced from absent (it is not
   vacuously green). The step is load-bearing (no `continue-on-error`) and reuses
   the kernel and rootfs the fork-correctness phase already staged.

## Custom tightened profile: evaluated and declined

The issue asks whether to ship a custom seccomp profile, scoped to the syscalls
Firecracker needs on Mitos's paths, BEYOND the upstream default. We evaluated this
and declined, for now, with a clear re-open condition.

Considered: pass `--seccomp-filter <path>` with a hand-authored BPF allowlist
narrower than upstream's advanced filter.

Why declined:

- **No demonstrated reduction.** Upstream's advanced filter is already an
  allowlist scoped to the syscalls the VMM needs, with argument-level filtering on
  the sensitive ones. Mitos runs Firecracker on its supported paths (boot,
  snapshot/restore, vsock, virtio-rng, networking), all of which the upstream
  filter already covers. We have no identified syscall that the upstream filter
  allows but Mitos's paths never use, so a tighter filter would remove nothing we
  can name.

- **Brittle and version-coupled.** A custom filter that is even slightly too tight
  SIGSYS-kills a legitimate VMM the first time a kernel, libc, or Firecracker
  version change alters the syscall pattern (for example a new `clock_gettime`
  vDSO fallback, a different `mmap`/`madvise` shape, or an added `io_uring` path).
  That is a fragile, high-maintenance surface that fails CLOSED in the worst way:
  it breaks running sandboxes on an upgrade. The upstream filter is updated in
  lockstep with the VMM that needs it; a forked filter is not.

- **The upstream filter is the maintained, tested artifact.** Asserting that it is
  installed (mode 2) and never disabled is a stronger, cheaper guarantee than
  forking it.

Re-open condition: if a concrete required-syscall delta is ever identified (a
syscall the upstream filter permits that Mitos provably never needs, with a named
threat it would close), pass an explicit `--seccomp-filter` with a Mitos profile
derived from the upstream advanced filter minus that syscall, and add a KVM CI
assertion that the VMM still boots and forks under it. Until then, the decision is
to assert the upstream filter holds rather than maintain a parallel one.
