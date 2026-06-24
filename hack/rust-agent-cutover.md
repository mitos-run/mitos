# Rust guest agent: cutover complete (Phase E, #310)

The Rust guest agent (`guest/agent-rs`) is the SOLE production guest agent since
Phase E (#310). The Go agent (`guest/agent`) and the legacy JSON vsock protocol
(port 52) are removed from the codebase.

## What changed in Phase E

- `guest/agent/` deleted: the Go guest agent is gone.
- `bench/agent-conformance/` deleted: the cross-agent comparison harness is gone;
  there is only one agent.
- `internal/vsock/`: the JSON `Client`, `Connect`, `ConnectUnix`, `StreamConn`,
  `DialStream`, `DialStreamUnix`, and their streaming methods are removed. The
  `AgentPort` (52) constant is removed. The gRPC dial helpers (`DialGRPCConn`,
  `DialGRPCConnUnix`, `DialGRPCOverConn`, `AgentGRPCPort`) and the shared DTO
  types (`ExecResponse`, `ExecStreamFrame`, `VitalsResponse`, `NotifyForkedNetwork`,
  `VolumeMountEntry`, `NotifyForkedResponse`, etc.) remain: the gRPC host code
  still uses these structs as plain Go types.
- `guest/rootfs/build.sh`: the `AGENT_IMPL` selector and the Go build branch are
  removed. The script builds only the Rust agent.
- KVM CI: the conformance-harness paths trigger removed from `kvm-test.yaml`.

## What the Rust agent serves

The Rust agent serves ONLY gRPC on vsock port 53 (AgentGRPCPort). All host-side
callers speak gRPC. There is no JSON fallback and no Go agent fallback.

## Security-sensitive reviewer policy

The Rust agent codebase (`guest/agent-rs`) requires a named human reviewer before
any PR touching the following paths is merged to main:

- `guest/agent-rs/src/sys/`
- `guest/agent-rs/src/fork/`
- `guest/agent-rs/src/init/mod.rs`
- `guest/agent-rs/src/main.rs`

See `docs/threat-model.md` and `docs/security-review-policy.md`.
