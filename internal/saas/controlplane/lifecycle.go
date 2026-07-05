package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
)

// maxLifecycleRespBytes bounds a buffered lifecycle response body. The daemon
// pause/resume replies are tiny JSON objects; the cap only guards against a
// misbehaving endpoint.
const maxLifecycleRespBytes = 1 << 20 // 1 MiB

// lifecycleBody names the sandbox a pause or resume acts on. It mirrors the
// daemon request shape (internal/daemon/lifecycle_api.go): the id rides in the
// JSON body, not the path, which is why these ops cannot share the Connect
// runtime proxy.
type lifecycleBody struct {
	Sandbox string `json:"sandbox"`
}

// lifecycle forwards a pause or resume (route "/v1/pause" or "/v1/resume") to
// the org-owned sandbox's runtime endpoint. The sandbox id comes from the JSON
// body; the sandbox is read org-scoped with the org-label re-check, so a
// missing or cross-org id collapses to not_found and is NEVER forwarded. The
// request presented upstream carries the per-sandbox bearer token (never
// logged, never echoed) and the body {"sandbox": <resolved name>}; the
// upstream status and body are passed through so the daemon's own error stays
// actionable.
func (k *K8sControlPlane) lifecycle(ctx context.Context, req saas.ForwardRequest, route string) (saas.ForwardResponse, error) {
	var body lifecycleBody
	if err := json.Unmarshal(req.Body, &body); err != nil || body.Sandbox == "" {
		return errResp(apierr.Get(apierr.CodeInvalidInput).
			WithCause(`the request body does not name a sandbox; send {"sandbox": "<id>"} with the id of the sandbox to act on`)), nil
	}

	// Org-scoped read with the org-label re-check: a missing or cross-org sandbox
	// collapses to not_found and never reaches the endpoint.
	sb, ok := k.getOwned(ctx, req.OrgID, body.Sandbox)
	if !ok {
		return notFound(body.Sandbox), nil
	}
	// Fork-aware endpoint and token resolution: a fromSandbox fork carries its
	// endpoint and (reissued) token on its first CHILD, never on the fork object.
	endpoint := runtimeEndpoint(sb)
	if endpoint == "" {
		return errResp(apierr.Get(apierr.CodeNotFound).
			WithCause(fmt.Sprintf("sandbox %q has no runtime endpoint yet; it is not Ready", body.Sandbox))), nil
	}

	token, err := k.readSandboxToken(ctx, sb)
	if err != nil {
		return errResp(apierr.Get(apierr.CodeInternal).
			WithCause("the per-sandbox access token secret could not be read")), nil
	}

	// The upstream body carries the control-plane-resolved sandbox name, never a
	// client-supplied value the resolution did not verify.
	payload, err := json.Marshal(map[string]string{"sandbox": runtimeSandboxID(sb)})
	if err != nil {
		return errResp(apierr.Get(apierr.CodeInternal).
			WithCause("could not encode the lifecycle request")), nil
	}
	outReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+endpoint+route, bytes.NewReader(payload))
	if err != nil {
		return errResp(apierr.Get(apierr.CodeInternal).
			WithCause("could not build the lifecycle proxy request")), nil
	}
	outReq.Header.Set("Content-Type", "application/json")
	outReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := k.httpClient.Do(outReq)
	if err != nil {
		return errResp(withStatus(apierr.Get(apierr.CodeInternal).
			WithCause("the sandbox runtime endpoint could not be reached"), http.StatusBadGateway)), nil
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxLifecycleRespBytes))
	if err != nil {
		return errResp(withStatus(apierr.Get(apierr.CodeInternal).
			WithCause("the sandbox runtime endpoint response could not be read"), http.StatusBadGateway)), nil
	}

	header := http.Header{}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		header.Set("Content-Type", ct)
	}
	return saas.ForwardResponse{
		Status: resp.StatusCode,
		Header: header,
		Body:   respBody,
	}, nil
}
