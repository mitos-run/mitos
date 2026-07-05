package controlplane

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
)

// connectService is the Connect service the sandbox serves runtime calls on.
const connectService = "/sandbox.v1.Sandbox/"

// proxy reverse-proxies a runtime call (exec, files, run_code over Connect) to
// the org-owned sandbox endpoint, attaching the per-sandbox bearer token and the
// X-Sandbox-Id header. The sandbox id is resolved from the X-Sandbox-Id header or
// the request path; the sandbox is read org-scoped and its org label re-checked,
// so a cross-org id returns not_found and is NEVER proxied. The request and
// response bodies are streamed, not buffered, so ExecStream is carried.
func (k *K8sControlPlane) proxy(ctx context.Context, req saas.ForwardRequest) (saas.ForwardResponse, error) {
	method := connectMethod(req)
	if method == "" {
		return errResp(apierr.Get(apierr.CodeNotFound).
			WithCause("the runtime path does not name a sandbox.v1.Sandbox method")), nil
	}

	id := sandboxIDFromRuntime(req)
	if id == "" {
		return errResp(apierr.Get(apierr.CodeNotFound).
			WithCause("no sandbox id was supplied; set the X-Sandbox-Id header or use /v1/sandboxes/<id>/<verb>")), nil
	}

	// Org-scoped read with the org-label re-check: a missing or cross-org sandbox
	// collapses to not_found and never reaches the endpoint.
	sb, ok := k.getOwned(ctx, req.OrgID, id)
	if !ok {
		return notFound(id), nil
	}
	// A multi-child fork fan-out cannot be addressed by one id: refuse it typed
	// rather than silently routing everything to child 0. This guard runs
	// BEFORE the terminal gate so a fan-out whose first child happens to be
	// reaped still gets the single-child limitation, never child 0's
	// idle_timeout speaking for the whole fan-out.
	if aerr := multiChildRuntimeError(sb); aerr != nil {
		return errResp(*aerr), nil
	}
	// Terminal phases answer with the typed error BEFORE any dial: the claim
	// keeps its stale endpoint after the VM stopped (issue #688), and dialing
	// it would surface a generic 502 where docs/lifecycle.md promises the typed
	// idle_timeout error for a reaped sandbox.
	if aerr := terminalRuntimeError(sb); aerr != nil {
		return errResp(*aerr), nil
	}
	// Fork-aware endpoint and token resolution: a fromSandbox fork carries its
	// endpoint and (reissued) token on its first CHILD, never on the fork object.
	endpoint := runtimeEndpoint(sb)
	if endpoint == "" {
		return errResp(apierr.Get(apierr.CodeNotFound).
			WithCause(fmt.Sprintf("sandbox %q has no runtime endpoint yet; it is not Ready", id))), nil
	}

	token, err := k.readSandboxToken(ctx, sb)
	if err != nil {
		return errResp(apierr.Get(apierr.CodeInternal).
			WithCause("the per-sandbox access token secret could not be read")), nil
	}

	body := req.BodyStream
	if body == nil {
		body = strings.NewReader("")
		if len(req.Body) > 0 {
			body = strings.NewReader(string(req.Body))
		}
	}

	target := "http://" + endpoint + connectService + method
	httpMethod := req.Method
	if httpMethod == "" {
		httpMethod = http.MethodPost
	}
	outReq, err := http.NewRequestWithContext(ctx, httpMethod, target, body)
	if err != nil {
		return errResp(apierr.Get(apierr.CodeInternal).WithCause("could not build the runtime proxy request")), nil
	}

	// Copy only the curated headers the gateway forwarded, then OVERWRITE the
	// credential: the client Authorization is stripped (the gateway already
	// excluded it) and replaced with the per-sandbox token, and X-Sandbox-Id is
	// set from the control-plane-resolved name, never trusted from the client.
	copyRuntimeHeaders(outReq.Header, req.Header)
	outReq.Header.Set("Authorization", "Bearer "+token)
	outReq.Header.Set("X-Sandbox-Id", runtimeSandboxID(sb))

	resp, err := k.httpClient.Do(outReq)
	if err != nil {
		return errResp(withStatus(apierr.Get(apierr.CodeInternal).
			WithCause("the sandbox runtime endpoint could not be reached"), http.StatusBadGateway)), nil
	}

	// Stream the response back: the body is handed to the gateway as a stream and
	// closed there, so a long-lived Connect response is never buffered.
	header := http.Header{}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		header.Set("Content-Type", ct)
	}
	return saas.ForwardResponse{
		Status:     resp.StatusCode,
		Header:     header,
		BodyStream: resp.Body,
	}, nil
}

// connectMethod returns the Connect method name from the runtime request: the
// last path segment of a /sandbox.v1.Sandbox/<Method> path, or the verb mapped
// from a /v1/sandboxes/<id>/<verb> alias.
func connectMethod(req saas.ForwardRequest) string {
	path := req.Path
	if i := strings.Index(path, connectService); i >= 0 {
		m := path[i+len(connectService):]
		if m != "" && !strings.Contains(m, "/") {
			return m
		}
		return ""
	}
	p := strings.TrimPrefix(path, "/v1/")
	if !strings.HasPrefix(p, "sandboxes/") {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(p, "sandboxes/"), "/")
	if len(parts) < 2 {
		return ""
	}
	switch parts[1] {
	case "exec":
		return "Exec"
	case "files":
		return "Files"
	case "run_code":
		return "RunCode"
	default:
		return ""
	}
}

// sandboxIDFromRuntime resolves the target sandbox id: the X-Sandbox-Id header
// (native Connect path) or the /v1/sandboxes/<id>/<verb> path segment.
func sandboxIDFromRuntime(req saas.ForwardRequest) string {
	if req.Header != nil {
		if id := req.Header.Get("X-Sandbox-Id"); id != "" {
			return id
		}
	}
	p := strings.TrimPrefix(req.Path, "/v1/")
	if strings.HasPrefix(p, "sandboxes/") {
		parts := strings.Split(strings.TrimPrefix(p, "sandboxes/"), "/")
		if len(parts) >= 1 && parts[0] != "" {
			return parts[0]
		}
	}
	return ""
}

// copyRuntimeHeaders copies the content-negotiation headers from the gateway's
// curated set onto the outbound request. It deliberately never copies
// Authorization (the credential is set by the caller from the per-sandbox token).
func copyRuntimeHeaders(dst, src http.Header) {
	if src == nil {
		return
	}
	for _, key := range []string{"Content-Type", "Accept", "Connect-Protocol-Version", "Connect-Timeout-Ms"} {
		if v := src.Get(key); v != "" {
			dst.Set(key, v)
		}
	}
}

// compile-time assertion that K8sControlPlane satisfies the seam.
var _ saas.ControlPlane = (*K8sControlPlane)(nil)
