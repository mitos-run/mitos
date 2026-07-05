package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/tenant"
)

// forkBody is the POST /v1/sandboxes/{id}/fork request shape. The Python SDK
// (DirectSandbox._fork_one) sends {"id": <child>, "template": <pool>,
// "pause_source": true}: id and template are accepted for compatibility but the
// control plane names the child itself and derives the template from the
// SOURCE's pool (a live fork boots from the source's memory, never from a
// caller-named template). replicas and count are recognized only to be
// rejected honestly when they ask for more than one child.
type forkBody struct {
	ID          string `json:"id,omitempty"`
	Template    string `json:"template,omitempty"`
	PauseSource bool   `json:"pause_source,omitempty"`
	Replicas    int32  `json:"replicas,omitempty"`
	Count       int32  `json:"count,omitempty"`
}

// fork serves the hosted live fork (issue #709): it resolves the ORG-OWNED
// source sandbox from the request path, verifies it is Ready (a live fork
// copies the source VM's running memory), and submits a Sandbox whose source is
// FromSandbox naming it (NO PoolRef), which the controller's fork engine
// (#611) drives. The response is create-shaped (id, endpoint, phase, token,
// template_id, fork_time_ms) so the flat SDK needs no change.
//
// Security: the org id is taken ONLY from req.OrgID (the gateway verified it
// from the customer key). The source is read org-scoped with the org-label
// re-check (getOwned), so a fork naming another org's sandbox id collapses to
// not_found, indistinguishable from a missing id, and NEVER forks it.
func (k *K8sControlPlane) fork(ctx context.Context, req saas.ForwardRequest) (saas.ForwardResponse, error) {
	startedAt := k.now()

	id := forkSourceID(req.Path)
	if id == "" {
		return errResp(apierr.Get(apierr.CodeNotFound).
			WithCause("no source sandbox id in the fork request path")), nil
	}

	var body forkBody
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return errResp(apierr.Get(apierr.CodeInvalidJSON).
				WithCause("the fork request body is not valid JSON")), nil
		}
	}

	// The gateway fork response is create-shaped: ONE id, endpoint, and token.
	// The Sandbox spec does support an N-replica fromSandbox fan-out, but its N
	// children (each with its own endpoint and reissued token) cannot be
	// represented on this route, so more than one child is rejected honestly
	// rather than silently returning only the first child of N.
	if body.Replicas > 1 || body.Count > 1 {
		return errResp(apierr.Get(apierr.CodeInvalidInput).
			WithCause("the fork route creates exactly one child per call; a multi-replica fan-out cannot be represented in its single-sandbox response").
			WithRemediation("Call POST /v1/sandboxes/<id>/fork once per child (the SDK's fork(n) does this), or create a Sandbox with spec.source.fromSandbox and spec.replicas through the Kubernetes API for an indexed fan-out.")), nil
	}

	// Fast-fail on a missing or cross-org source (the #646 pool fast-fail
	// precedent): waiting can never help, and the controller would only fail the
	// fork terminally after the caller blocked for the full ready timeout. A
	// missing id and another org's id answer identically (no oracle).
	src, ok := k.getOwned(ctx, req.OrgID, id)
	if !ok {
		return notFound(id), nil
	}

	// A live fork copies the source VM's running memory, so the source must be
	// Ready NOW: anything else is a state conflict, answered instantly with the
	// phase and what to do about it.
	if src.Status.Phase != v1.SandboxReady {
		return errResp(withStatus(apierr.Get(apierr.CodeInvalidInput).
			WithMessage(fmt.Sprintf("the fork source sandbox is not running (phase %s)", phaseOrUnknown(src))).
			WithCause(fmt.Sprintf("sandbox %q is in phase %s; a live fork copies the source VM's running memory, so the source must be Ready", id, phaseOrUnknown(src))).
			WithRemediation("Wait for the source sandbox to reach phase Ready (poll GET /v1/sandboxes/<id>) and retry the fork, or fork a different Ready sandbox."), http.StatusConflict)), nil
	}

	ns := k.namespaceForOrg(req.OrgID)
	name := generateName()
	sb := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    tenant.OrgLabels(req.OrgID),
		},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{
				FromSandbox: &v1.FromSandboxSource{
					Name:        src.Name,
					PauseSource: body.PauseSource,
				},
			},
		},
	}

	if err := k.c.Create(ctx, sb); err != nil {
		switch {
		case apierrors.IsAlreadyExists(err):
			return errResp(withStatus(apierr.Get(apierr.CodeInternal).
				WithCause("a sandbox with the generated name already exists; retry"), http.StatusConflict)), nil
		case isNamespaceMissing(err, ns):
			return errResp(namespaceMissingErr(req.OrgID, ns)), nil
		case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
			return errResp(apierr.Get(apierr.CodeInvalidInput).
				WithCause("the api server rejected the fork object as invalid: " + err.Error())), nil
		default:
			return errResp(apierr.Get(apierr.CodeInternal).
				WithCause("could not create the fork object")), nil
		}
	}

	return k.pollReady(ctx, ns, name, startedAt)
}

// forkSourceID extracts the SOURCE sandbox id from a fork path
// (/v1/sandboxes/<id>/fork): the segment before the trailing "fork" verb. It
// mirrors idFromPath's segment handling.
func forkSourceID(path string) string {
	trimmed := strings.Trim(path, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 || parts[len(parts)-1] != "fork" {
		return ""
	}
	return parts[len(parts)-2]
}
