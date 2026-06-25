package saas

import (
	"context"
	"io"
	"net/http"
)

// QuotaEnforcer is the seam the quota and rate-limit workstream (issue #213)
// implements. The gateway calls Check after it has authenticated the key and
// resolved the org, before forwarding to the control plane. This slice ships a
// default-allow implementation (AllowAllQuota) so the gateway is end-to-end
// testable now; #213 swaps in the real enforcer without touching the gateway.
type QuotaEnforcer interface {
	// Check decides whether a request from org may proceed. op names the public
	// operation (for example "sandbox.create"). A nil error means allow; a
	// non-nil error means deny, and the gateway maps it to the public
	// quota_exceeded envelope.
	Check(ctx context.Context, orgID, op string) error
}

// AllowAllQuota is the default QuotaEnforcer: it allows every request. It is the
// stand-in until issue #213 lands a real enforcer.
type AllowAllQuota struct{}

// Check always allows.
func (AllowAllQuota) Check(_ context.Context, _ string, _ string) error { return nil }

// ForwardRequest is the org-scoped, authenticated request the gateway hands to
// the control plane. OrgID is attached by the gateway after key verification and
// is the ONLY org the forward target may act on; the target must scope every
// effect to it. Op is the public operation; Body is the opaque request payload.
//
// Method and Header carry the original request method and a curated subset of the
// request headers, populated by the gateway for the runtime-proxy op
// (sandbox.runtime): the control plane reverse-proxies the request to the
// sandbox endpoint and needs the method (Connect is POST) and the
// X-Sandbox-Id / Content-Type headers. For the lifecycle ops (create, status,
// list, terminate) these may be empty; the control plane reads Body and Path.
//
// BodyStream, when non-nil, is the unbuffered request body the control plane MUST
// stream to the sandbox (this carries ExecStream, which must not be buffered).
// When BodyStream is nil the control plane uses Body. The gateway sets exactly
// one for a runtime op and Body for the lifecycle ops.
type ForwardRequest struct {
	OrgID      string
	Op         string
	Path       string
	Method     string
	Header     http.Header
	Body       []byte
	BodyStream io.Reader
}

// ForwardResponse is the control plane's reply. Status is an HTTP-style status
// the gateway echoes; Body is the opaque buffered response payload (used by the
// lifecycle ops). Header carries response headers the gateway echoes (the
// runtime proxy sets Content-Type so a streamed Connect response is decodable).
//
// BodyStream, when non-nil, is an unbuffered response body the gateway streams to
// the caller and then closes (the runtime proxy sets it so a streaming Connect
// response is not buffered in the gateway). When BodyStream is nil the gateway
// writes Body.
type ForwardResponse struct {
	Status     int
	Header     http.Header
	Body       []byte
	BodyStream io.ReadCloser
}

// ControlPlane is the seam the gateway forwards authenticated, org-scoped
// requests to. The real implementation calls the controller (the claim path,
// internal/controller) over the internal mTLS plane, carrying the org as the
// tenant boundary. It is an interface so the gateway is unit-tested without a
// live control plane: a fake records the ForwardRequest and asserts the OrgID
// the gateway attached, which is how the cross-org isolation test proves a key
// for org A can never reach org B.
type ControlPlane interface {
	Forward(ctx context.Context, req ForwardRequest) (ForwardResponse, error)
}
