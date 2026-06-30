"""Fork-native subagents: spawning a subagent IS forking the current sandbox.

Use case hook: a multi-agent harness wants to spawn a subagent. When the harness
itself runs INSIDE a mitos sandbox, the differentiated move is to spawn that
subagent by forking the current warm sandbox: the child is a copy-on-write
snapshot of the live parent, so it starts warm with the parent's state, with no
cold boot and no external orchestrator round-trip. When the harness is NOT on
mitos, there is nothing to fork, so the hook returns a graceful no-op fallback
and the harness uses its normal (cold) spawn path. The fallback is first-class:
the hook never raises just because the code is off-platform.

    import mitos.subagent as sub

    result = sub.spawn_subagent(3, label="researcher")
    if result.on_mitos:
        for child in result.children:
            ...        # drive each warm subagent
    else:
        ...            # off mitos: spawn subagents the normal way

Detection signal: the in-guest self-service socket advertised at $MITOS_SOCKET
(mitos.guest). The host sets it at claim time only inside a mitos sandbox, so its
presence is the honest "am I running inside mitos" check. The fork is routed over
that socket (mitos.guest.fork): no network egress, no API credentials.

Honest scope: the warm fork needs a running mitos sandbox context. Off mitos
(running this on your laptop), spawn_subagent returns on_mitos=False, which is
exactly what this example prints. The budget-gated self-fork is wired
progressively by the guest agent (issue #25); where it is not yet enabled the
socket returns its remediation as a mitos.guest.GuestError, which is a real
on-mitos error rather than the off-mitos fallback.

Run::

    python3 fork_native_subagent.py      # off mitos: prints the fallback
    # inside a mitos sandbox: forks the current warm sandbox into subagents

Byte-compiled and import-checked by the sdk-examples CI job; not executed there
(no VM, and this is import-safe regardless of platform). The asserts below run at
import time as a real API-surface check.
"""

import mitos
import mitos.subagent as sub
from mitos.subagent import SubagentHandle, SubagentResult

# Drift guard: assert the surface the example uses, so a rename fails import.
assert callable(sub.spawn_subagent)
assert callable(sub.is_on_mitos)
assert callable(sub.current_sandbox)
assert {"on_mitos", "children", "reason"} <= set(SubagentResult.__dataclass_fields__)
assert hasattr(SubagentResult, "first")  # the convenience property
assert {"sandbox_id", "label", "on_mitos"} <= set(SubagentHandle.__dataclass_fields__)
assert mitos.subagent is sub  # exported from the package root


def spawn_research_team(n: int = 3) -> SubagentResult:
    """Spawn n research subagents. On mitos each is a warm fork of this very
    sandbox; off mitos this is a no-op fallback the caller can branch on."""
    return sub.spawn_subagent(n, label="researcher")


def main() -> None:
    me = sub.current_sandbox()
    if me is not None:
        print(f"running inside mitos sandbox: {me.sandbox_id}")
    else:
        print("not running inside a mitos sandbox")

    result = spawn_research_team(3)
    if result.on_mitos:
        print(f"spawned {len(result.children)} warm subagents by forking the current sandbox:")
        for child in result.children:
            print(f"  warm subagent {child.sandbox_id} (label={child.label!r})")
        # A real harness would now drive each child (exec, run_code, ...). Driving
        # a child from another process needs control-plane credentials; that
        # reconnect helper is a documented follow-up.
    else:
        print(f"fork-native spawn unavailable: {result.reason}")
        print("falling back to the normal subagent spawn path here.")

    print("fork_native_subagent example OK")


if __name__ == "__main__":
    main()
