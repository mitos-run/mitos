package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
)

// templateDescriptor is the template shape the SDK and standalone sandbox-server
// agree on. The hosted path maps a named SandboxPool to this shape; the
// standalone path records the snapshot build result. JSON tags must match
// sandbox-server's templateInfo exactly so the SDK parses both consistently.
type templateDescriptor struct {
	ID        string    `json:"id"`
	Ready     bool      `json:"ready"`
	CreatedAt time.Time `json:"created_at"`
	TimeMs    float64   `json:"creation_time_ms"`
}

// ensureTemplateBody is the create-template request shape sent by the Python SDK's
// create_template / ensure_template call (POST /v1/templates).
type ensureTemplateBody struct {
	ID string `json:"id"`
}

// ensureTemplate handles the "template.ensure" op (POST /v1/templates). In the
// hosted model a template maps to a SandboxPool: the SDK calls ensure_template
// before forking to confirm the named pool is intended. This handler validates the
// request body and returns a ready descriptor without creating any object. Actual
// pool existence is checked by the controller when the caller forks: if no pool
// carries the requested name the resulting Sandbox lands in Failed with a clear
// human-readable condition message, which the SDK surfaces as an error. Auth and
// org context flow through the standard gateway pipeline unchanged; this handler
// adds no new privilege.
//
// An empty id is rejected 400. Any non-empty id is accepted and returns 200+ready
// so the SDK create() can proceed to fork.
func (k *K8sControlPlane) ensureTemplate(_ context.Context, req saas.ForwardRequest) (saas.ForwardResponse, error) {
	var body ensureTemplateBody
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return errResp(apierr.Get(apierr.CodeInvalidJSON).
				WithCause("the template request body is not valid JSON")), nil
		}
	}
	if body.ID == "" {
		return errResp(apierr.Get(apierr.CodeInvalidJSON).
			WithCause("template id is required: set \"id\" in the request body to the pool or image name")), nil
	}
	desc := templateDescriptor{
		ID:        body.ID,
		Ready:     true,
		CreatedAt: k.now(),
	}
	return jsonResp(http.StatusOK, desc), nil
}

// listTemplates handles the "template.list" op (GET /v1/templates). It lists all
// SandboxPools across all namespaces and maps each to a templateDescriptor so the
// response shape matches the standalone sandbox-server (GET /v1/templates returns
// []templateInfo). SandboxPools are cluster-wide shared infrastructure, not
// per-tenant data, so listing without a namespace filter is correct here.
// A pool with ReadySnapshots > 0 is reported as ready: true.
func (k *K8sControlPlane) listTemplates(ctx context.Context, _ saas.ForwardRequest) (saas.ForwardResponse, error) {
	var pools v1.SandboxPoolList
	if err := k.c.List(ctx, &pools); err != nil {
		return errResp(apierr.Get(apierr.CodeInternal).
			WithCause("could not list available templates")), nil
	}
	items := make([]templateDescriptor, 0, len(pools.Items))
	for i := range pools.Items {
		p := &pools.Items[i]
		items = append(items, templateDescriptor{
			ID:        p.Name,
			Ready:     p.Status.ReadySnapshots > 0,
			CreatedAt: p.CreationTimestamp.Time,
		})
	}
	return jsonResp(http.StatusOK, items), nil
}
