package controlplane

import (
	"context"
	"fmt"

	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
)

// K8sControlPlane satisfies the RuntimeResolver seam the gateway uses to proxy a
// PTY WebSocket. A compile-time check so the wiring cannot silently break.
var _ saas.RuntimeResolver = (*K8sControlPlane)(nil)

// ResolveRuntime resolves the org-scoped runtime target for a WebSocket proxy
// (the interactive PTY rides the Connect Exec RPC over a WebSocket, which the
// gateway must hijack rather than buffer through Forward). It mirrors proxy()'s
// preconditions exactly: the sandbox is read org-scoped with the org-label
// re-check, so a missing or cross-org id collapses to not_found and is NEVER
// proxied; it must be Ready (carry a runtime endpoint); and its per-sandbox token
// is read from the controller-owned <name>-sandbox-token Secret. The token VALUE
// is never logged. A nil *apierr.Error means success.
//
// This makes K8sControlPlane satisfy saas.RuntimeResolver.
func (k *K8sControlPlane) ResolveRuntime(ctx context.Context, orgID, id string) (saas.RuntimeTarget, *apierr.Error) {
	sb, ok := k.getOwned(ctx, orgID, id)
	if !ok {
		e := apierr.Get(apierr.CodeNotFound).
			WithCause(fmt.Sprintf("no sandbox %q exists for this organization", id))
		return saas.RuntimeTarget{}, &e
	}
	// Terminal phases answer with the typed error BEFORE any dial, mirroring
	// proxy(): a Terminated claim keeps its stale endpoint after the VM stopped
	// (issue #688), so the PTY must get the documented idle_timeout error, not
	// a WebSocket dial failure against a dead pod IP.
	if aerr := terminalRuntimeError(sb); aerr != nil {
		return saas.RuntimeTarget{}, aerr
	}
	// A multi-child fork fan-out cannot be addressed by one id: refuse it typed
	// rather than silently routing everything to child 0.
	if aerr := multiChildRuntimeError(sb); aerr != nil {
		return saas.RuntimeTarget{}, aerr
	}
	// Fork-aware endpoint and token resolution: a fromSandbox fork carries its
	// endpoint and (reissued) token on its first CHILD, never on the fork object.
	endpoint := runtimeEndpoint(sb)
	if endpoint == "" {
		e := apierr.Get(apierr.CodeNotFound).
			WithCause(fmt.Sprintf("sandbox %q has no runtime endpoint yet; it is not Ready", id))
		return saas.RuntimeTarget{}, &e
	}
	token, err := k.readSandboxToken(ctx, sb)
	if err != nil {
		e := apierr.Get(apierr.CodeInternal).
			WithCause("the per-sandbox access token secret could not be read")
		return saas.RuntimeTarget{}, &e
	}
	return saas.RuntimeTarget{Endpoint: endpoint, Token: token, SandboxID: runtimeSandboxID(sb)}, nil
}
