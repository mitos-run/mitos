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
	if sb.Status.Endpoint == "" {
		e := apierr.Get(apierr.CodeNotFound).
			WithCause(fmt.Sprintf("sandbox %q has no runtime endpoint yet; it is not Ready", id))
		return saas.RuntimeTarget{}, &e
	}
	token, err := k.readToken(ctx, sb.Namespace, sb.Name)
	if err != nil {
		e := apierr.Get(apierr.CodeInternal).
			WithCause("the per-sandbox access token secret could not be read")
		return saas.RuntimeTarget{}, &e
	}
	return saas.RuntimeTarget{Endpoint: sb.Status.Endpoint, Token: token, SandboxID: sb.Name}, nil
}
