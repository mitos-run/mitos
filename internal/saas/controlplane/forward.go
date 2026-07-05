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

// maxCreateReplicas is the inclusive upper bound on the replicas a single create
// request may ask for. It is a request-validation guard against a huge value
// churning the controller; real fleet size is separately bounded by controller
// replica admission and the replica-weighted live-usage cap (issue #733).
const maxCreateReplicas = 64

// Forward dispatches an authenticated, org-scoped request to the matching
// Kubernetes action or the runtime reverse proxy. orgID is taken ONLY from
// req.OrgID and bounds every effect: the namespace is NamespaceForOrg(orgID) and
// the org label is re-checked on every object, so a key for org A can never act
// on or reach org B.
func (k *K8sControlPlane) Forward(ctx context.Context, req saas.ForwardRequest) (saas.ForwardResponse, error) {
	switch req.Op {
	case "sandbox.create":
		return k.create(ctx, req)
	case "sandbox.fork":
		return k.fork(ctx, req)
	case "sandbox.status":
		return k.status(ctx, req)
	case "sandbox.list":
		return k.list(ctx, req)
	case "sandbox.terminate":
		return k.terminate(ctx, req)
	case "sandbox.runtime":
		return k.proxy(ctx, req)
	case "sandbox.pause":
		return k.lifecycle(ctx, req, "/v1/pause")
	case "sandbox.resume":
		return k.lifecycle(ctx, req, "/v1/resume")
	case "template.ensure":
		return k.ensureTemplate(ctx, req)
	case "template.list":
		return k.listTemplates(ctx, req)
	default:
		// An unmatched path reaches here as "sandbox.<method>" (opFromPath's
		// fallback), which is not a sandbox at all: override the not_found default
		// message ("no such sandbox") so the caller is not told a sandbox is
		// missing when they hit a route the gateway does not expose (issue #640C).
		return errResp(apierr.Get(apierr.CodeNotFound).
			WithMessage("no such route or operation").
			WithCause(fmt.Sprintf("the request did not map to a known gateway operation (resolved op %q)", req.Op)).
			WithRemediation("Use a documented route: POST or GET /v1/sandboxes, GET or DELETE /v1/sandboxes/<id>, POST /v1/sandboxes/<id>/fork, POST or GET /v1/templates, POST /v1/fork, or the runtime paths under /v1/sandboxes/<id>/.")), nil
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
	startedAt := k.now()
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
		// The body decoded fine; it just names no origin. That is a semantic
		// input error, not a JSON parse failure (issue #640B).
		return errResp(apierr.Get(apierr.CodeInvalidInput).
			WithCause("the create request sets none of pool, image, or template and the server has no default pool configured").
			WithRemediation("Set \"pool\" (or \"image\"/\"template\") in the request body to an existing pool; list pools with GET /v1/templates.")), nil
	}

	// Bound the requested replica count at request validation. Actual
	// materialization is already bounded by controller replica admission and the
	// replica-weighted live-usage cap, so this is not the only guard, but a huge
	// value still invites needless controller churn before those lower bounds
	// reject it. The cap is inclusive (issue #733, item 4).
	if body.Replicas > maxCreateReplicas {
		return errResp(apierr.Get(apierr.CodeInvalidInput).
			WithCause(fmt.Sprintf("replicas %d exceeds the per-request maximum of %d", body.Replicas, maxCreateReplicas)).
			WithRemediation(fmt.Sprintf("Request at most %d replicas per create; fork additional sandboxes if you need a larger fleet.", maxCreateReplicas))), nil
	}

	ns := k.namespaceForOrg(req.OrgID)

	// Fast-fail on an unknown pool. In the hosted control plane pools are
	// pre-provisioned and stable, so a create naming a pool that does not exist
	// in the tenant namespace is a typo, not a manifest-ordering race: return an
	// instant, LLM-legible 404 instead of creating a Sandbox that can only pend
	// until the controller's bounded grace expires (issue #630) while the caller
	// blocks for the full ready timeout and then sees a misleading "did not
	// become ready" 504. k.c is an uncached reader, so this Get is authoritative
	// for the exact namespace the sandbox would be created in and the controller
	// would resolve the poolRef in. A transient (non-NotFound) read error is NOT
	// treated as absent: the create proceeds and the controller's bounded grace
	// still governs the direct and GitOps-race paths.
	var poolObj v1.SandboxPool
	if err := k.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: pool}, &poolObj); err != nil && apierrors.IsNotFound(err) {
		return errResp(apierr.Get(apierr.CodeNotFound).
			WithMessage(fmt.Sprintf("no such pool %q", pool)).
			WithCause(fmt.Sprintf("no SandboxPool named %q exists in namespace %q", pool, ns)).
			WithRemediation("Check the pool name for a typo and use a pool listed by GET /v1/templates, or create the SandboxPool before launching a sandbox from it.")), nil
	}

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

	return k.pollReady(ctx, ns, name, startedAt)
}

// pollReady blocks until the sandbox reaches Ready (returns 201 + token), Failed
// (returns the rejection message), a terminal Rejected condition (409 with the
// controller's actionable message; the fork engine records it without a Failed
// phase, so waiting on the phase alone would misreport it as a timeout), or the
// readiness timeout (504-style). startedAt is when the create or fork op began;
// it feeds the observed-latency fallback in the ready payload.
func (k *K8sControlPlane) pollReady(ctx context.Context, ns, name string, startedAt time.Time) (saas.ForwardResponse, error) {
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
			return k.readyResponse(ctx, &sb, startedAt)
		case v1.SandboxFailed:
			return errResp(withStatus(apierr.Get(apierr.CodeInternal).
				WithCause("the sandbox failed to start: "+failureReason(&sb)), http.StatusBadGateway)), nil
		}

		// A terminal Rejected condition (the fork engine's secret-inheritance
		// default-deny) is recorded WITHOUT a Failed phase; surface the
		// controller's actionable message instead of pending into a timeout.
		// The secrets gate gets its own 403 whose remediation names the exact
		// wire-level opt-in, so an agent can self-correct in one round trip.
		if c := rejectedCondition(&sb); c != nil {
			if c.Reason == v1.ReasonSecretInheritanceDenied {
				return errResp(withStatus(apierr.Get(apierr.CodeForbidden).
					WithMessage("the fork was rejected: the source sandbox holds secrets").
					WithCause(c.Message).
					WithRemediation("A live fork duplicates the source VM's memory, including delivered secret values, so it is denied by default. Re-request the fork with \"secret_inheritance\": \"inherit\" in the body to explicitly permit duplicating them into the child, or fork a sandbox that holds no secrets."), http.StatusForbidden)), nil
			}
			return errResp(withStatus(apierr.Get(apierr.CodeInvalidInput).
				WithMessage("the sandbox was rejected by the controller").
				WithCause(c.Message), http.StatusConflict)), nil
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
// payload. The token is returned ONLY here and is never logged. It serves BOTH
// origins: a pool claim (source.poolRef, endpoint and token on the object
// itself) and a live fork (source.fromSandbox, whose endpoint and token live on
// the fork's first CHILD; the gateway fork route creates single-child forks).
//
// The response includes template_id (the pool the sandbox descends from; for a
// fork, the SOURCE's pool) and fork_time_ms so the Python SDK DirectSandbox
// constructor can parse it without a KeyError: SandboxServer.fork and
// DirectSandbox._fork_one both read data["template_id"] and
// data["fork_time_ms"] from the JSON body.
func (k *K8sControlPlane) readyResponse(ctx context.Context, sb *v1.Sandbox, startedAt time.Time) (saas.ForwardResponse, error) {
	endpoint := runtimeEndpoint(sb)
	token, err := k.readSandboxToken(ctx, sb)
	if err != nil {
		return errResp(apierr.Get(apierr.CodeInternal).
			WithCause("the sandbox is ready but its access token secret could not be read")), nil
	}
	templateID := k.templateID(ctx, sb)
	payload := map[string]any{
		"id":           sb.Name,
		"endpoint":     endpoint,
		"token":        token,
		"phase":        string(v1.SandboxReady),
		"template_id":  templateID,
		"fork_time_ms": forkTimeMs(sb, startedAt, k.now()),
	}
	resp := jsonResp(http.StatusCreated, payload)
	// Echo the non-identifying pool name so the gateway's telemetry can attach
	// it as the pool property on sandbox.created and sandbox.forked events.
	if templateID != "" {
		resp.Header.Set("X-Mitos-Pool", templateID)
	}
	return resp, nil
}

// forkTimeMs is the honest fork latency for the ready payload: the
// engine-measured startup latency the controller recorded (a pool claim stamps
// status.startupLatencyMs; a live fork stamps the child's startupLatencyMs),
// falling back to the control plane's own observed submit-to-Ready wall time
// when the engine value is absent. Never a hardcoded zero.
func forkTimeMs(sb *v1.Sandbox, startedAt, now time.Time) float64 {
	if sb.Status.StartupLatencyMs > 0 {
		return float64(sb.Status.StartupLatencyMs)
	}
	if len(sb.Status.Children) > 0 && sb.Status.Children[0].StartupLatencyMs > 0 {
		return float64(sb.Status.Children[0].StartupLatencyMs)
	}
	elapsed := now.Sub(startedAt)
	if elapsed < 0 {
		elapsed = 0
	}
	return float64(elapsed.Milliseconds())
}

// templateID is the pool name the sandbox descends from: the poolRef for a
// claim, or the SOURCE's poolRef for a fromSandbox fork (best-effort: a source
// deleted after the fork completed yields ""). The SDK stores it as the child's
// template.
func (k *K8sControlPlane) templateID(ctx context.Context, sb *v1.Sandbox) string {
	if sb.Spec.Source.PoolRef != nil {
		return sb.Spec.Source.PoolRef.Name
	}
	if src := sb.Spec.Source.FromSandbox; src != nil {
		var parent v1.Sandbox
		if err := k.c.Get(ctx, client.ObjectKey{Namespace: sb.Namespace, Name: src.Name}, &parent); err == nil {
			if parent.Spec.Source.PoolRef != nil {
				return parent.Spec.Source.PoolRef.Name
			}
		}
	}
	return ""
}

// runtimeEndpoint is the address runtime traffic for sb targets: the object's
// own endpoint for a pool claim, or the first fork child's endpoint for a
// fromSandbox fork (the fork object is the fan-out record; the controller
// stamps endpoints on status.children, never on the fork object itself).
func runtimeEndpoint(sb *v1.Sandbox) string {
	if sb.Status.Endpoint != "" {
		return sb.Status.Endpoint
	}
	if len(sb.Status.Children) > 0 {
		return sb.Status.Children[0].Endpoint
	}
	return ""
}

// tokenSecretNameFor is the controller-owned token Secret for runtime access to
// sb: <name>-sandbox-token for a pool claim; for a fromSandbox fork the tokens
// are per CHILD (<child>-sandbox-token, reissued so the source's token never
// opens a fork), and the gateway fork route creates single-child forks, so the
// first child's Secret is the one.
func tokenSecretNameFor(sb *v1.Sandbox) string {
	if sb.Spec.Source.FromSandbox != nil && len(sb.Status.Children) > 0 {
		return sb.Status.Children[0].Name + tokenSecretSuffix
	}
	return sb.Name + tokenSecretSuffix
}

// multiChildRuntimeError refuses the gateway runtime surface for a fromSandbox
// fork with MORE than one child. The gateway's own fork route creates
// single-child forks, but a fork object created by another client (the cluster
// SDK, kubectl) can fan out to N children, each with its own endpoint and
// reissued token; silently routing every call to child 0 would misdirect
// traffic and leave children 1..N-1 unreachable. Nil means the surface can
// serve sb.
func multiChildRuntimeError(sb *v1.Sandbox) *apierr.Error {
	if sb.Spec.Source.FromSandbox != nil && len(sb.Status.Children) > 1 {
		e := apierr.Get(apierr.CodeInvalidInput).
			WithMessage("the sandbox is a multi-child fork fan-out, which this API cannot address").
			WithCause(fmt.Sprintf("sandbox %q has %d fork children; this surface serves single-child forks only and will not silently pick one", sb.Name, len(sb.Status.Children))).
			WithRemediation("Address each child directly through the Kubernetes API (status.children carries per-child endpoints and each child has its own token Secret), or create single-child forks with POST /v1/sandboxes/<id>/fork.")
		return &e
	}
	return nil
}

// runtimeSandboxID is the sandbox id the DAEMON serving runtimeEndpoint(sb)
// knows the VM by: the object name for a pool claim, or the first child's
// engine-registered id for a fromSandbox fork. It rides X-Sandbox-Id and the
// lifecycle body so raw-forkd (a shared per-node endpoint that routes by id)
// addresses the right VM; a single-sandbox husk endpoint ignores it.
func runtimeSandboxID(sb *v1.Sandbox) string {
	if sb.Status.Endpoint == "" && len(sb.Status.Children) > 0 && sb.Status.Children[0].SandboxID != "" {
		return sb.Status.Children[0].SandboxID
	}
	return sb.Name
}

// readSandboxToken reads the bearer token for runtime access to sb from its
// controller-owned Secret (fork-aware via tokenSecretNameFor).
func (k *K8sControlPlane) readSandboxToken(ctx context.Context, sb *v1.Sandbox) (string, error) {
	return k.readToken(ctx, sb.Namespace, tokenSecretNameFor(sb))
}

// readToken reads data.token from the named controller-owned Secret in the
// sandbox namespace.
func (k *K8sControlPlane) readToken(ctx context.Context, ns, secretName string) (string, error) {
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: ns, Name: secretName}
	if err := k.c.Get(ctx, key, &secret); err != nil {
		return "", fmt.Errorf("read token secret %s/%s: %w", ns, key.Name, err)
	}
	tok := string(secret.Data["token"])
	if tok == "" {
		return "", fmt.Errorf("token secret %s/%s has no token", ns, key.Name)
	}
	return tok, nil
}

// rejectedCondition returns the controller's terminal Rejected condition on
// the sandbox, or nil when none is recorded. Callers branch on the Reason
// (SecretInheritanceDenied gets its own public shape).
func rejectedCondition(sb *v1.Sandbox) *metav1.Condition {
	for i := range sb.Status.Conditions {
		c := &sb.Status.Conditions[i]
		if c.Type == v1.ConditionRejected && c.Status == metav1.ConditionTrue {
			return c
		}
	}
	return nil
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
// carries the token. The endpoint is fork-aware (runtimeEndpoint), so a live
// fork's status names the child endpoint runtime traffic actually targets.
func sandboxSummary(sb *v1.Sandbox) map[string]any {
	return map[string]any{
		"id":        sb.Name,
		"phase":     string(sb.Status.Phase),
		"endpoint":  runtimeEndpoint(sb),
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
