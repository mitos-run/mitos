package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/tenant"
)

// forkBody is the POST /v1/sandboxes/{id}/fork request shape. The Python SDK
// (DirectSandbox._fork_one) sends {"id": <child>, "template": <pool>,
// "pause_source": true}: a valid id names the child (DNS-1123, pre-validated);
// an absent id lets the control plane generate one. template is accepted for
// compatibility but the template is derived from the SOURCE's pool (a live
// fork boots from the source's memory, never from a caller-named template).
// secret_inheritance is the explicit opt-in for duplicating the source's
// in-memory secrets into the child ("reissue" is the controller's default:
// the fork gets fresh credentials). replicas and count are recognized only to
// be rejected honestly when they ask for more than one child.
type forkBody struct {
	ID                string `json:"id,omitempty"`
	Template          string `json:"template,omitempty"`
	PauseSource       bool   `json:"pause_source,omitempty"`
	SecretInheritance string `json:"secret_inheritance,omitempty"`
	Replicas          int32  `json:"replicas,omitempty"`
	Count             int32  `json:"count,omitempty"`
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

	// Validate the caller's inputs BEFORE any cluster read or write, so a bad
	// request is an instant typed 400, never an object that pends into a
	// timeout or a raw api server validation error.
	if body.SecretInheritance != "" && body.SecretInheritance != string(v1.SecretReissue) && body.SecretInheritance != string(v1.SecretInherit) {
		return errResp(apierr.Get(apierr.CodeInvalidInput).
			WithCause(fmt.Sprintf("secret_inheritance %q is not a mode", body.SecretInheritance)).
			WithRemediation("Use \"reissue\" (the default: the fork gets fresh credentials) or \"inherit\" (explicit opt-in: the fork duplicates the source's in-memory secrets).")), nil
	}
	if body.ID != "" {
		if errs := validation.IsDNS1123Subdomain(body.ID); len(errs) > 0 {
			return errResp(apierr.Get(apierr.CodeInvalidInput).
				WithCause(fmt.Sprintf("the requested fork id %q is not a valid name: %s", body.ID, errs[0])).
				WithRemediation("Use lowercase alphanumerics and hyphens (RFC 1123), or omit id to have one generated.")), nil
		}
	}

	// Fast-fail on a missing or cross-org source (the #646 pool fast-fail
	// precedent): waiting can never help, and the controller would only fail the
	// fork terminally after the caller blocked for the full ready timeout. A
	// missing id and another org's id answer identically (no oracle).
	src, ok := k.getOwned(ctx, req.OrgID, id)
	if !ok {
		return notFound(id), nil
	}

	// A source that is itself a fromSandbox fork is rejected honestly: the
	// running VM is the fork's CHILD, while a new FromSandbox spec would name
	// the fork OBJECT, and the controller's fork-of-fork resolution is not
	// proven on this surface (its templateID would also be empty). Until it is,
	// the honest answer is a typed reject with a path forward, not an
	// unverified fork that can only pend into a timeout.
	if src.Spec.Source.FromSandbox != nil {
		return errResp(apierr.Get(apierr.CodeInvalidInput).
			WithMessage("forking a fork is not supported on this API yet").
			WithCause(fmt.Sprintf("sandbox %q is itself a live fork (source.fromSandbox); this route serves pool-descended sources only", id)).
			WithRemediation("Fork the original pool-created sandbox again, or create a fresh sandbox from the pool, bring it to the state you want, and fork that.")), nil
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
	name := body.ID
	if name == "" {
		name = generateName()
	}
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
	if body.SecretInheritance != "" {
		sb.Spec.SecretInheritance = v1.SecretInheritanceMode(body.SecretInheritance)
	}

	if err := k.c.Create(ctx, sb); err != nil {
		switch {
		case apierrors.IsAlreadyExists(err):
			if body.ID != "" {
				return errResp(withStatus(apierr.Get(apierr.CodeInvalidInput).
					WithCause(fmt.Sprintf("a sandbox named %q already exists in this organization", name)).
					WithRemediation("Choose a different id, or omit id to have one generated."), http.StatusConflict)), nil
			}
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

	// Timing split for the fork latency investigation. submitToCreateMs is the
	// control plane's own cost to persist the fork Sandbox (request receipt to a
	// successful Create); submitToReadyMs is the full server-side wall the client
	// waits on (request receipt to the ready watch observing the child Ready), so
	// the difference is the controller scheduling + fork work + watch delivery.
	// This is the authoritative, client-correlated server-side fork latency: it
	// does NOT depend on the controller's "fork timing complete" totalMs, which
	// measures to the end of the reconcile pass and overstates the client-observed
	// latency (it can keep counting after the ready watch already returned).
	// Purely observational: it does not change the fork control flow.
	createdAt := k.now()
	resp, err := k.pollReady(ctx, ns, name, startedAt)
	// ok discriminates the latency stream: pollReady reports ready-timeouts and
	// failed sandboxes as an error ENVELOPE with a nil error, so err alone would
	// count a timed-out wait as served and skew every percentile derived here.
	slog.Info("fork served",
		"sandbox", name,
		"submitToCreateMs", createdAt.Sub(startedAt).Milliseconds(),
		"submitToReadyMs", k.now().Sub(startedAt).Milliseconds(),
		"ok", err == nil && resp.Status < 400,
		"status", resp.Status,
	)
	return resp, err
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
