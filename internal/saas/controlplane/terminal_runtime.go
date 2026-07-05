package controlplane

import (
	"fmt"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/apierr"
)

// terminalRuntimeError returns the typed, LLM-legible error a runtime call
// (exec, files, run_code, and the PTY WebSocket) must receive when the target
// sandbox is in a terminal phase, or nil when the sandbox is live. Both
// runtime paths (proxy and ResolveRuntime) consult it BEFORE dialing: a
// terminal claim keeps its stale Status.Endpoint in both run modes (the
// raw-forkd VM is reaped on its node; the husk pod is deleted at lifetime
// expiry, issue #688), so dialing it can only produce a generic 502 where
// docs/lifecycle.md promises the typed idle_timeout error for a reaped
// sandbox. The gate reads the claim PHASE, upstream of any mode-specific
// endpoint, so raw-forkd and husk-backed sandboxes answer identically.
//
// A Terminated sandbox returns the documented idle_timeout catalogue entry
// (410 Gone), with the message and remediation tailored to the reap reason
// terminateLifetime recorded (IdleTimeout, MaxLifetimeExceeded, or
// TimeoutExpired). A Failed sandbox never ran out a lifetime, so it returns
// not_found with the failure detail from its Ready condition as the cause:
// actionable, and honest that nothing is running to call. Condition messages
// are controller-authored and never carry secret values.
func terminalRuntimeError(sb *v1.Sandbox) *apierr.Error {
	switch sb.Status.Phase {
	case v1.SandboxTerminated:
		reason := terminatedConditionReason(sb)
		e := apierr.Get(apierr.CodeIdleTimeout)
		switch reason {
		case "MaxLifetimeExceeded":
			e = e.WithMessage("the sandbox was reaped after exceeding its max lifetime").
				WithRemediation("The sandbox hit its max lifetime and was reaped; create a fresh sandbox (or fork from a checkpoint) and retry. Set a longer lifetime on the sandbox at create time if the work needs more wall-clock time.")
		case "TimeoutExpired":
			e = e.WithMessage("the sandbox was reaped after its live set_timeout deadline expired").
				WithRemediation("The set_timeout deadline expired and the sandbox was reaped; create a fresh sandbox and retry, or call set_timeout with a later deadline before it passes.")
		}
		if reason == "" {
			reason = "Terminated"
		}
		e = e.WithCause(fmt.Sprintf("sandbox %q reached the terminal Terminated phase (%s); its VM is stopped and the object remains readable until deleted", sb.Name, reason))
		return &e
	case v1.SandboxFailed:
		e := apierr.Get(apierr.CodeNotFound).
			WithMessage("the sandbox failed and is not running").
			WithCause(fmt.Sprintf("sandbox %q reached the terminal Failed phase: %s", sb.Name, failureReason(sb))).
			WithRemediation("The sandbox failed terminally and will never serve runtime calls; inspect the failure cause, then create a new sandbox and retry.")
		return &e
	}
	// A fromSandbox fork's running VM is its CHILD, and the lifetime reaper
	// flips the CHILD phase while the fork object stays Ready, so the parent
	// phase alone would pass a reaped fork straight to a dead child endpoint
	// and a generic 502 (the #688 dead-end class). Consult the child the
	// runtime surface targets. Only a SINGLE-child fork is interpreted here:
	// a multi-child fan-out is refused by multiChildRuntimeError, which every
	// runtime call site runs FIRST, and answering with child 0's terminal
	// state would wrongly speak for the other children.
	if sb.Spec.Source.FromSandbox != nil && len(sb.Status.Children) == 1 {
		child := sb.Status.Children[0]
		switch child.Phase {
		case v1.SandboxTerminated:
			e := apierr.Get(apierr.CodeIdleTimeout).
				WithCause(fmt.Sprintf("fork child %q of sandbox %q reached the terminal Terminated phase; its VM is stopped and the fork object remains readable until deleted", child.Name, sb.Name)).
				WithRemediation("The fork child was reaped (idle timeout or lifetime expiry); fork a Ready sandbox again and retry. Set a longer lifetime at fork time if the work needs more wall-clock time.")
			return &e
		case v1.SandboxFailed:
			e := apierr.Get(apierr.CodeNotFound).
				WithMessage("the sandbox failed and is not running").
				WithCause(fmt.Sprintf("fork child %q of sandbox %q reached the terminal Failed phase", child.Name, sb.Name)).
				WithRemediation("The fork child failed terminally and will never serve runtime calls; fork a Ready sandbox again and retry.")
			return &e
		}
	}
	return nil
}

// terminatedConditionReason returns the reason from the sandbox's Terminated
// condition (stamped by the controller's lifetime reaper), or "".
func terminatedConditionReason(sb *v1.Sandbox) string {
	for i := range sb.Status.Conditions {
		if sb.Status.Conditions[i].Type == "Terminated" {
			return sb.Status.Conditions[i].Reason
		}
	}
	return ""
}
