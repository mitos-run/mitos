package saas

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"mitos.run/mitos/internal/apierr"
)

// maxBodyBytes bounds a forwarded request body so a customer cannot exhaust the
// gateway with an unbounded upload. It mirrors the daemon sandbox API ceiling.
const maxBodyBytes = 8 << 20 // 8 MiB

// opRuntime is the public operation for a runtime proxy call (exec, files,
// run_code over the Connect sandbox.v1.Sandbox service). It is reverse-proxied to
// the sandbox endpoint by the control plane, not handled as a lifecycle verb. It
// requires the sandbox scope because it acts inside a running sandbox.
const opRuntime = "sandbox.runtime"

// runtimeMethod returns the Connect method name and true when path is a runtime
// proxy call, otherwise "", false. Two shapes are accepted: the native Connect
// path /sandbox.v1.Sandbox/<Method>, and the friendly
// /v1/sandboxes/<id>/{exec,files,run_code} alias. The control plane resolves the
// sandbox id from the X-Sandbox-Id header (native) or the path (alias).
func runtimeMethod(path string) (string, bool) {
	const connectPrefix = "/sandbox.v1.Sandbox/"
	if strings.HasPrefix(path, connectPrefix) {
		m := strings.TrimPrefix(path, connectPrefix)
		if m != "" && !strings.Contains(m, "/") {
			return m, true
		}
		return "", false
	}
	p := strings.TrimPrefix(path, "/v1/")
	if !strings.HasPrefix(p, "sandboxes/") {
		return "", false
	}
	rest := strings.TrimPrefix(p, "sandboxes/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" {
		return "", false
	}
	switch parts[1] {
	case "exec":
		return "Exec", true
	case "files":
		return "Files", true
	case "run_code":
		return "RunCode", true
	default:
		return "", false
	}
}

// requiredScopeFor maps a public operation to the scope a key must carry. A
// read-only op is satisfied by either scope; a mutating op requires the sandbox
// scope. An unknown op requires the sandbox scope (fail closed).
func requiredScopeFor(op string) string {
	switch op {
	case "sandbox.list", "sandbox.status":
		return ScopeReadOnly
	default:
		return ScopeSandboxes
	}
}

// Gateway is the public, customer-facing front door. It terminates customer key
// authentication, resolves the owning organization, attaches an org context,
// enforces quota through the QuotaEnforcer seam, and forwards to the control
// plane through the ControlPlane seam. It maps every internal failure to the
// public LLM-legible error envelope (internal/apierr); a key for org A can never
// reach org B's resources because the OrgID the gateway forwards is taken solely
// from the verified key, never from the request.
//
// The gateway NEVER logs a key value. It logs the resolved key id, masked
// prefix, org id, and op only.
type Gateway struct {
	keys  *KeyService
	quota QuotaEnforcer
	cp    ControlPlane
	log   *slog.Logger
}

// NewGateway builds a gateway. A nil quota enforcer defaults to AllowAllQuota
// (the #213 seam). A nil logger discards logs.
func NewGateway(keys *KeyService, quota QuotaEnforcer, cp ControlPlane, log *slog.Logger) *Gateway {
	if quota == nil {
		quota = AllowAllQuota{}
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Gateway{keys: keys, quota: quota, cp: cp, log: log}
}

// opFromPath derives the public operation name from the request path and method.
// The gateway exposes a flat surface under /v1/sandboxes plus the Connect runtime
// service; the op is used for scope selection, quota accounting, and forwarding.
// A runtime proxy call (Connect path or the /v1/sandboxes/<id>/{exec,files,
// run_code} alias) maps to opRuntime; the lifecycle verbs map as before.
func opFromPath(method, path string) string {
	if _, ok := runtimeMethod(path); ok {
		return opRuntime
	}
	p := strings.TrimPrefix(path, "/v1/")
	switch {
	case p == "sandboxes" && method == http.MethodGet:
		return "sandbox.list"
	case p == "sandboxes" && method == http.MethodPost:
		return "sandbox.create"
	case strings.HasPrefix(p, "sandboxes/") && method == http.MethodGet:
		return "sandbox.status"
	case strings.HasPrefix(p, "sandboxes/") && method == http.MethodDelete:
		return "sandbox.terminate"
	default:
		return "sandbox." + strings.ToLower(method)
	}
}

// ServeHTTP is the gateway handler. The pipeline is: extract the bearer key;
// verify it (shape, hash, expiry, revocation, scope) in constant time; resolve
// the org from the verified key; enforce quota; forward to the control plane
// with the org attached; and echo the control-plane response. Every failure
// becomes a public error envelope.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	op := opFromPath(r.Method, r.URL.Path)
	scope := requiredScopeFor(op)

	raw, ok := bearerToken(r)
	if !ok {
		g.fail(w, apierr.Get(apierr.CodeUnauthorized).
			WithCause("no bearer api key was presented"))
		return
	}

	res, err := g.keys.Verify(r.Context(), raw, scope)
	if err != nil {
		g.failVerify(w, err)
		return
	}
	orgID := res.Key.OrgID

	// Quota is enforced AFTER authn and org-resolution, BEFORE forwarding, so a
	// denied request never touches the control plane. The real enforcer (issue
	// #213) distinguishes a quota cap (quota_exceeded) from a rate-limit denial
	// (rate_limited) and a suspension (forbidden): when the enforcer error supplies
	// its own envelope via the APIError seam, the gateway honors it; otherwise it
	// falls back to the generic quota_exceeded envelope.
	if qErr := g.quota.Check(r.Context(), orgID, op); qErr != nil {
		g.log.Info("gateway quota denied", "key_id", res.Key.ID, "org", orgID, "op", op)
		g.fail(w, quotaEnvelope(qErr, op))
		return
	}

	// The OrgID forwarded is taken ONLY from the verified key. The request body
	// and path cannot influence it, so a key for org A cannot address org B.
	fwd := ForwardRequest{OrgID: orgID, Op: op, Path: r.URL.Path, Method: r.Method}

	if op == opRuntime {
		// Runtime (exec, files, run_code over Connect) is reverse-proxied and may
		// carry an unbounded stream (ExecStream), so the body is NOT buffered or
		// size-capped here; it is streamed by the control plane. Only a curated set
		// of headers crosses the seam, and the client Authorization is NEVER
		// forwarded: the control plane attaches the per-sandbox token itself.
		fwd.BodyStream = r.Body
		fwd.Header = curatedRuntimeHeaders(r.Header)
	} else {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
		if err != nil {
			g.fail(w, apierr.Get(apierr.CodeInternal).WithCause("read request body failed"))
			return
		}
		if len(body) > maxBodyBytes {
			g.fail(w, apierr.Get(apierr.CodeBodyTooLarge).
				WithCause("the forwarded request body exceeds the gateway limit"))
			return
		}
		fwd.Body = body
	}

	g.log.Info("gateway forward", "key_id", res.Key.ID, "key_prefix", res.Key.Prefix, "org", orgID, "op", op)

	resp, err := g.cp.Forward(r.Context(), fwd)
	if err != nil {
		g.fail(w, apierr.Get(apierr.CodeInternal).
			WithCause("the control plane could not service the request"))
		return
	}
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	// A control-plane response may carry its own headers (the runtime proxy sets
	// Content-Type so a streamed Connect body is decodable). Default to JSON for
	// the lifecycle ops.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	if resp.BodyStream != nil {
		// Stream the response without buffering (this carries a streamed Connect
		// response), then close the upstream body.
		defer func() { _ = resp.BodyStream.Close() }()
		_, _ = io.Copy(w, resp.BodyStream)
		return
	}
	_, _ = w.Write(resp.Body)
}

// curatedRuntimeHeaders copies only the headers the runtime proxy needs across
// the control-plane seam. The client Authorization is deliberately EXCLUDED: the
// control plane attaches the per-sandbox bearer token, so a customer cannot
// influence the credential presented to the sandbox.
func curatedRuntimeHeaders(in http.Header) http.Header {
	out := http.Header{}
	for _, k := range []string{"X-Sandbox-Id", "Content-Type", "Accept", "Connect-Protocol-Version", "Connect-Timeout-Ms"} {
		if v := in.Get(k); v != "" {
			out.Set(k, v)
		}
	}
	return out
}

// failVerify maps a key-verification error to the public envelope. A missing,
// malformed, or unknown key is unauthorized (a probe cannot distinguish them); an
// expired or revoked key is unauthorized; a scope or wrong-org failure is
// forbidden (the credential is valid but the action is not allowed for it).
func (g *Gateway) failVerify(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrKeyScope), errors.Is(err, ErrKeyWrongOrg):
		g.fail(w, apierr.Get(apierr.CodeForbidden).
			WithCause("the key is valid but not permitted for this action"))
	default:
		// Malformed, unknown, expired, revoked all collapse to unauthorized so the
		// response does not reveal which one applies.
		g.fail(w, apierr.Get(apierr.CodeUnauthorized).
			WithCause("the api key is missing, invalid, expired, or revoked"))
	}
}

// apiErrorProvider is the seam a quota enforcer error implements to carry its own
// public envelope. The real enforcer (internal/saas/quota, issue #213) returns a
// denial that names the precise code (quota_exceeded, rate_limited, or
// forbidden); the gateway honors that code instead of collapsing every denial to
// quota_exceeded. An error that does not implement this falls back to the generic
// quota_exceeded envelope, which is correct for the cap case.
type apiErrorProvider interface {
	APIError() apierr.Error
}

// quotaEnvelope maps a quota-enforcer denial to its public envelope. If the error
// carries its own envelope (via apiErrorProvider, possibly wrapped), that
// envelope is used; otherwise the generic quota_exceeded envelope is returned.
// The envelope never includes a secret value.
func quotaEnvelope(qErr error, op string) apierr.Error {
	var p apiErrorProvider
	if errors.As(qErr, &p) {
		e := p.APIError()
		if e.Context == nil {
			e = e.WithContext(map[string]any{"op": op})
		}
		return e
	}
	return apierr.Get(apierr.CodeQuotaExceeded).
		WithCause("organization quota check denied the request").
		WithContext(map[string]any{"op": op})
}

// fail writes the error envelope. It never includes any secret value.
func (g *Gateway) fail(w http.ResponseWriter, e apierr.Error) {
	apierr.Encode(w, e)
}

// bearerToken extracts a bearer token from the Authorization header. It returns
// the raw token and whether one was present.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}
