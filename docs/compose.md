# Compose provider contract

Status: the host-side contract is DEFINED and unit-proven. The in-guest backend
that actually runs `docker compose` is a separate, hardware/kernel gated
follow-up and does NOT exist yet. Nothing here claims that a compose stack runs
end to end today.

## Why

Mitos runs one workload per microVM today. Many agent evaluation harnesses
(Harbor: Terminal-Bench, SWE-Bench, and similar) define their environment as a
`docker-compose.yaml`: a main agent container plus sidecars (databases, mock
servers, MCP servers). To be a valid compose provider for that task class, a
provider must advertise a `docker_compose` capability and implement per-service
operations against arbitrary services, not just the main one, plus collect hooks
that gather sidecar artifacts at teardown (for example a `pg_dump` from a
database sidecar before grading).

This is the compose epic. The work is sequenced:

- A0 guest kernel with container features built in (`=Y`) plus `iptables-legacy`.
- A1 in-guest privileged `dockerd` plus `docker compose` on a dedicated overlay2
  device (main plus sidecars).
- A2 (this contract) the Harbor compose provider contract: `docker_compose=true`,
  per-service exec/copy/stop, collect hooks.
- A3 warm-image compose snapshots.
- A4 live-fork a warm compose stack (the differentiator).

A0 and A1 are hardware/kernel gated and out of scope here. This page documents
only A2, the contract.

## What is defined and proven today

`internal/compose` is the host-side contract and router for the per-service
operations. It provides:

- `Capabilities{DockerCompose bool}`, advertised true ONLY when a working backend
  is wired. With no backend, the flag is false: the capability is never claimed
  dishonestly.
- Typed per-service requests addressing a named compose service: `ServiceExec`,
  `ServiceDownloadFile`, `ServiceDownloadDir` (with exclusions), `ServiceIsDir`,
  `StopService`.
- `CollectHook` plus `Collect`, which run each teardown hook against its sidecar,
  gather every hook's output independently, and never abort the batch on a single
  hook's failure (each hook's error is recorded in its own result).
- Input validation that runs BEFORE any backend dispatch:
  - service names are constrained to `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$` (no
    slashes, no leading dot, no `..`, no whitespace), so a service name can never
    escape into a path element;
  - container paths must be absolute with no `..` segment, mirroring the
    traversal defense on the sandbox file APIs.
- A `Backend` interface that abstracts the actual compose execution, and an
  `UnavailableBackend` default that fails closed.

The contract, routing, validation, traversal rejection, collect orchestration,
and the fail-closed path are all unit-tested against a mock backend in
`internal/compose/compose_test.go`. These tests need no KVM, no dockerd, and no
running guest.

## What is gated (not done here)

The real `Backend` is the in-guest privileged `dockerd` plus `docker compose`
(issues #489 and #490). It does not exist yet. Until it is wired:

- `UnavailableBackend` is the default;
- `Capabilities().DockerCompose` reports `false`;
- every per-service operation returns `ErrBackendUnavailable`, an error that
  names the missing backend and tells the operator to enable it.

So per-service compose operations cannot drive any execution today. They fail
closed with a clear, actionable error. When the in-guest backend lands under
#489 and #490, it implements the `Backend` interface and the same contract,
routing, validation, and collect orchestration carry over unchanged. The threat
model row for the resulting exec and file-read surface (`docs/threat-model.md`,
section 3) is re-derived at that point.
