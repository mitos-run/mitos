package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/tenant"
)

// tokenSecretSuffix mirrors the controller's per-sandbox token Secret name
// (internal/controller/token_secret.go): <sandbox-name>-sandbox-token, with keys
// token and endpoint.
const tokenSecretSuffix = "-sandbox-token"

// Forward dispatches an authenticated, org-scoped request to the matching
// Kubernetes action or the runtime reverse proxy. orgID is taken ONLY from
// req.OrgID and bounds every effect: the namespace is NamespaceForOrg(orgID) and
// the org label is re-checked on every object, so a key for org A can never act
// on or reach org B.
func (k *K8sControlPlane) Forward(ctx context.Context, req saas.ForwardRequest) (saas.ForwardResponse, error) {
	switch req.Op {
	case "sandbox.create":
		return k.create(ctx, req)
	case "sandbox.status":
		return k.status(ctx, req)
	case "sandbox.list":
		return k.list(ctx, req)
	case "sandbox.terminate":
		return k.terminate(ctx, req)
	case "sandbox.runtime":
		return k.proxy(ctx, req)
	case "template.ensure":
		return k.ensureTemplate(ctx, req)
	case "template.list":
		return k.listTemplates(ctx, req)
	default:
		return errResp(apierr.Get(apierr.CodeNotFound).
			WithCause(fmt.Sprintf("unknown operation %q", req.Op))), nil
	}
}

// createBody is the create request shape. The origin pool is resolved in priority
// order: pool, then image, then template (the SDK fork body field), then the
// server-configured default. env, secrets, ttl/timeout, workspace, and replicas
// are optional.
//
// The template field matches the fork body sent by the Python SDK
// (SandboxServer.fork, DirectSandbox._fork_one): POST /v1/fork carries
// {"template":"<pool-name>","id":"<sandbox-id>"}. Accepting template here lets
// both POST /v1/sandboxes and POST /v1/fork reach the same handler after
// opFromPath maps them both to sandbox.create.
type createBody struct {
	Pool      string            `json:"pool,omitempty"`
	Image     string            `json:"image,omitempty"`
	Template  string            `json:"template,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Secrets   []secretMountReq  `json:"secrets,omitempty"`
	Timeout   string            `json:"timeout,omitempty"`
	TTL       string            `json:"ttl,omitempty"`
	Workspace string            `json:"workspace,omitempty"`
	Replicas  int32             `json:"replicas,omitempty"`
}

// secretMountReq is the create request's secret-mount shape, mapped onto
// v1.SecretMount.
type secretMountReq struct {
	Name      string `json:"name"`
	SecretRef struct {
		Name string `json:"name"`
		Key  string `json:"key"`
	} `json:"secretRef"`
	EnvVar    string `json:"envVar,omitempty"`
	MountPath string `json:"mountPath,omitempty"`
}

// create builds and submits a Sandbox in the org namespace, then polls it to a
// terminal create outcome. On Ready it returns 201 with the id, endpoint, phase,
// and the per-sandbox token (returned ONLY here). On Failed it returns the
// rejection condition as an LLM-legible 4xx; on timeout a 504-style error.
func (k *K8sControlPlane) create(ctx context.Context, req saas.ForwardRequest) (saas.ForwardResponse, error) {
	var body createBody
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return errResp(apierr.Get(apierr.CodeInvalidJSON).
				WithCause("the create request body is not valid JSON")), nil
		}
	}

	pool := body.Pool
	if pool == "" {
		pool = body.Image
	}
	if pool == "" {
		pool = body.Template
	}
	if pool == "" {
		pool = k.defaultPool
	}
	if pool == "" {
		return errResp(apierr.Get(apierr.CodeInvalidJSON).
			WithCause("the create request names neither a pool nor an image and no default pool is configured")), nil
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
				PoolRef: &v1.LocalObjectReference{Name: pool},
			},
		},
	}
	if body.Replicas > 1 {
		sb.Spec.Replicas = body.Replicas
	}
	if len(body.Env) > 0 {
		sb.Spec.Env = make([]corev1.EnvVar, 0, len(body.Env))
		for kk, vv := range body.Env {
			sb.Spec.Env = append(sb.Spec.Env, corev1.EnvVar{Name: kk, Value: vv})
		}
	}
	for _, sm := range body.Secrets {
		sb.Spec.Secrets = append(sb.Spec.Secrets, v1.SecretMount{
			Name: sm.Name,
			SecretRef: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: sm.SecretRef.Name},
				Key:                  sm.SecretRef.Key,
			},
			EnvVar:    sm.EnvVar,
			MountPath: sm.MountPath,
		})
	}
	if body.Workspace != "" {
		sb.Spec.WorkspaceRef = &v1.LocalObjectReference{Name: body.Workspace}
	}
	if d, ok := parseDuration(body.TTL, body.Timeout); ok {
		sb.Spec.Lifetime = &v1.SandboxLifetime{TTL: &metav1.Duration{Duration: d}}
	}

	if err := k.c.Create(ctx, sb); err != nil {
		switch {
		case apierrors.IsAlreadyExists(err):
			return errResp(withStatus(apierr.Get(apierr.CodeInternal).
				WithCause("a sandbox with the generated name already exists; retry"), http.StatusConflict)), nil
		case isNamespaceMissing(err, ns):
			return errResp(namespaceMissingErr(req.OrgID, ns)), nil
		case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
			// The caller's JSON decoded fine; the OBJECT the gateway built from
			// it failed api server validation (for example an org id that is not
			// a valid label value, #593). Surface the validation message instead
			// of blaming the request body, per the LLM-legible error rule (#28).
			return errResp(apierr.Get(apierr.CodeInvalidInput).
				WithCause("the api server rejected the sandbox object as invalid: " + err.Error())), nil
		default:
			return errResp(apierr.Get(apierr.CodeInternal).
				WithCause("could not create the sandbox object")), nil
		}
	}

	return k.pollReady(ctx, ns, name, req.OrgID)
}

// pollReady blocks until the sandbox reaches Ready (returns 201 + token), Failed
// (returns the rejection message), or the readiness timeout (504-style).
func (k *K8sControlPlane) pollReady(ctx context.Context, ns, name, orgID string) (saas.ForwardResponse, error) {
	deadline := k.now().Add(k.readyTimeout)
	ticker := time.NewTicker(k.pollInterval)
	defer ticker.Stop()

	for {
		var sb v1.Sandbox
		if err := k.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sb); err != nil {
			if apierrors.IsNotFound(err) {
				// The object vanished mid-create (a terminate raced the poll).
				return errResp(apierr.Get(apierr.CodeNotFound).
					WithCause("the sandbox was removed before it became ready")), nil
			}
			return errResp(apierr.Get(apierr.CodeInternal).
				WithCause("could not read the sandbox status while waiting for readiness")), nil
		}

		switch sb.Status.Phase {
		case v1.SandboxReady:
			return k.readyResponse(ctx, &sb)
		case v1.SandboxFailed:
			return errResp(withStatus(apierr.Get(apierr.CodeInternal).
				WithCause("the sandbox failed to start: "+failureReason(&sb)), http.StatusBadGateway)), nil
		}

		if !k.now().Before(deadline) {
			return errResp(withStatus(apierr.Get(apierr.CodeInternal).
				WithCause(fmt.Sprintf("the sandbox did not become ready within %s; it is still %s", k.readyTimeout, phaseOrUnknown(&sb))), http.StatusGatewayTimeout)), nil
		}

		select {
		case <-ctx.Done():
			return errResp(apierr.Get(apierr.CodeCanceled).
				WithCause("the create request was canceled while waiting for the sandbox to become ready")), nil
		case <-ticker.C:
		}
	}
}

// readyResponse reads the per-sandbox token Secret and returns the 201 create
// payload. The token is returned ONLY here and is never logged.
//
// The response includes template_id (the pool name the sandbox was forked from)
// and fork_time_ms so the Python SDK DirectSandbox constructor can parse it
// without a KeyError: SandboxServer.fork and DirectSandbox._fork_one both read
// data["template_id"] and data["fork_time_ms"] from the JSON body.
func (k *K8sControlPlane) readyResponse(ctx context.Context, sb *v1.Sandbox) (saas.ForwardResponse, error) {
	endpoint := sb.Status.Endpoint
	token, err := k.readToken(ctx, sb.Namespace, sb.Name)
	if err != nil {
		return errResp(apierr.Get(apierr.CodeInternal).
			WithCause("the sandbox is ready but its access token secret could not be read")), nil
	}
	poolName := ""
	if sb.Spec.Source.PoolRef != nil {
		poolName = sb.Spec.Source.PoolRef.Name
	}
	payload := map[string]any{
		"id":           sb.Name,
		"endpoint":     endpoint,
		"token":        token,
		"phase":        string(v1.SandboxReady),
		"template_id":  poolName,
		"fork_time_ms": 0.0,
	}
	return jsonResp(http.StatusCreated, payload), nil
}

// readToken reads data.token from the controller-owned <name>-sandbox-token
// Secret in the sandbox namespace.
func (k *K8sControlPlane) readToken(ctx context.Context, ns, name string) (string, error) {
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: ns, Name: name + tokenSecretSuffix}
	if err := k.c.Get(ctx, key, &secret); err != nil {
		return "", fmt.Errorf("read token secret %s/%s: %w", ns, key.Name, err)
	}
	tok := string(secret.Data["token"])
	if tok == "" {
		return "", fmt.Errorf("token secret %s/%s has no token", ns, key.Name)
	}
	return tok, nil
}

// status returns the org-scoped sandbox status. A sandbox that does not exist OR
// carries a different org label returns not_found, so another org's existence is
// never revealed.
func (k *K8sControlPlane) status(ctx context.Context, req saas.ForwardRequest) (saas.ForwardResponse, error) {
	id := idFromPath(req.Path)
	if id == "" {
		return errResp(apierr.Get(apierr.CodeNotFound).WithCause("no sandbox id in the request path")), nil
	}
	sb, ok := k.getOwned(ctx, req.OrgID, id)
	if !ok {
		return notFound(id), nil
	}
	return jsonResp(http.StatusOK, sandboxSummary(sb)), nil
}

// list returns every sandbox in the org namespace carrying the org label.
func (k *K8sControlPlane) list(ctx context.Context, req saas.ForwardRequest) (saas.ForwardResponse, error) {
	ns := k.namespaceForOrg(req.OrgID)
	var sbs v1.SandboxList
	err := k.c.List(ctx, &sbs,
		client.InNamespace(ns),
		client.MatchingLabels(tenant.OrgLabels(req.OrgID)),
	)
	if err != nil {
		if isNamespaceMissing(err, ns) {
			return errResp(namespaceMissingErr(req.OrgID, ns)), nil
		}
		return errResp(apierr.Get(apierr.CodeInternal).WithCause("could not list sandboxes for the organization")), nil
	}
	items := make([]map[string]any, 0, len(sbs.Items))
	for i := range sbs.Items {
		// Defense in depth: the namespace AND the label selector already bound the
		// list to this org; re-checking the label keeps the invariant local.
		if sbs.Items[i].Labels[tenant.OrgLabelKey] != req.OrgID {
			continue
		}
		items = append(items, sandboxSummary(&sbs.Items[i]))
	}
	return jsonResp(http.StatusOK, map[string]any{"sandboxes": items}), nil
}

// terminate deletes an org-owned sandbox. A missing or cross-org id returns
// not_found and never deletes another org's object.
func (k *K8sControlPlane) terminate(ctx context.Context, req saas.ForwardRequest) (saas.ForwardResponse, error) {
	id := idFromPath(req.Path)
	if id == "" {
		return errResp(apierr.Get(apierr.CodeNotFound).WithCause("no sandbox id in the request path")), nil
	}
	sb, ok := k.getOwned(ctx, req.OrgID, id)
	if !ok {
		return notFound(id), nil
	}
	if err := k.c.Delete(ctx, sb); err != nil {
		if apierrors.IsNotFound(err) {
			return notFound(id), nil
		}
		return errResp(apierr.Get(apierr.CodeInternal).WithCause("could not terminate the sandbox")), nil
	}
	return saas.ForwardResponse{Status: http.StatusNoContent}, nil
}

// getOwned reads the named sandbox from the org namespace and verifies its org
// label. It returns (nil, false) for a missing object OR an org-label mismatch,
// collapsing both to not_found so a cross-org probe cannot map ids. The org id is
// taken ONLY from the verified request.
func (k *K8sControlPlane) getOwned(ctx context.Context, orgID, name string) (*v1.Sandbox, bool) {
	ns := k.namespaceForOrg(orgID)
	var sb v1.Sandbox
	if err := k.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sb); err != nil {
		return nil, false
	}
	if sb.Labels[tenant.OrgLabelKey] != orgID {
		// The object lives in the org namespace but is not labeled for this org:
		// never act on it, never reveal it.
		return nil, false
	}
	return &sb, true
}

// sandboxSummary is the per-sandbox JSON in status and list responses. It never
// carries the token.
func sandboxSummary(sb *v1.Sandbox) map[string]any {
	return map[string]any{
		"id":        sb.Name,
		"phase":     string(sb.Status.Phase),
		"endpoint":  sb.Status.Endpoint,
		"createdAt": sb.CreationTimestamp.UTC().Format(time.RFC3339),
	}
}

// failureReason extracts the rejection message from a Failed sandbox's Ready
// condition so the create error is actionable. It never carries a secret.
func failureReason(sb *v1.Sandbox) string {
	for i := range sb.Status.Conditions {
		c := sb.Status.Conditions[i]
		if c.Type == "Ready" && c.Message != "" {
			return c.Message
		}
	}
	return "no failure detail was reported by the controller"
}

func phaseOrUnknown(sb *v1.Sandbox) string {
	if sb.Status.Phase == "" {
		return "Pending"
	}
	return string(sb.Status.Phase)
}

// idFromPath returns the last non-empty path segment as the sandbox id.
func idFromPath(path string) string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return ""
	}
	parts := strings.Split(trimmed, "/")
	return parts[len(parts)-1]
}

// parseDuration prefers ttl, then timeout, returning the parsed duration and true
// when one is a valid Go duration.
func parseDuration(ttl, timeout string) (time.Duration, bool) {
	for _, s := range []string{ttl, timeout} {
		if s == "" {
			continue
		}
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d, true
		}
	}
	return 0, false
}

// generateName returns a sandbox name "sb-" plus 8 hex chars of crypto entropy.
func generateName() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "sb-" + hex.EncodeToString(b)
}

// isNamespaceMissing reports whether err indicates the org namespace does not
// exist (a create or list raced ahead of provisioning). The api server reports a
// missing target namespace as a NotFound whose status details name the
// namespaces kind and the namespace name.
func isNamespaceMissing(err error, ns string) bool {
	if !apierrors.IsNotFound(err) {
		return false
	}
	if d := statusDetails(err); d != nil && d.Kind == "namespaces" && d.Name == ns {
		return true
	}
	return false
}

// statusDetails returns the k8s status details when err is an APIStatus error.
func statusDetails(err error) *metav1.StatusDetails {
	var s apierrors.APIStatus
	if errors.As(err, &s) {
		return s.Status().Details
	}
	return nil
}

// namespaceMissingErr is the actionable error when the org namespace is absent.
func namespaceMissingErr(orgID, ns string) apierr.Error {
	return withStatus(apierr.Get(apierr.CodeInternal).
		WithCause(fmt.Sprintf("the namespace %s for organization %s does not exist yet; it is provisioned when the organization is onboarded", ns, orgID)),
		http.StatusServiceUnavailable)
}

// notFound is the canonical org-isolation not_found: a missing or cross-org id.
func notFound(id string) saas.ForwardResponse {
	return errResp(apierr.Get(apierr.CodeNotFound).
		WithCause(fmt.Sprintf("no sandbox %q exists for this organization", id)))
}
