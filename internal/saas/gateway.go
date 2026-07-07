package saas

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/telemetry"
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
	case "sandbox.list", "sandbox.status", "template.list":
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
	// trustedHops is the number of trusted reverse-proxy hops in front of the
	// gateway. It governs how the client IP is resolved for the per-IP rate-limit
	// bucket: zero means X-Forwarded-For is never trusted (use RemoteAddr). See
	// TrustedProxyHops.
	trustedHops TrustedProxyHops
	tel         *telemetry.Emitter
	// metrics counts request outcomes and auth denials for the #617 SaaS
	// alerts. Nil (the default) disables all observation.
	metrics *GatewayMetrics
}

// NewGateway builds a gateway. A nil quota enforcer defaults to AllowAllQuota
// (the #213 seam). A nil logger discards logs. A nil telemetry emitter is a
// no-op (telemetry is opt-in and off by default); when supplied and enabled, the
// gateway emits a sandbox.created product event on a successful create. The
// emitter never receives a raw org id (it hashes it) and the gateway attaches
// only non-PII properties. The client-IP trust model defaults to zero trusted
// proxy hops (X-Forwarded-For is not trusted); use WithTrustedProxyHops to
// configure it when the gateway sits behind an ingress.
func NewGateway(keys *KeyService, quota QuotaEnforcer, cp ControlPlane, log *slog.Logger, opts ...GatewayOption) *Gateway {
	if quota == nil {
		quota = AllowAllQuota{}
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	g := &Gateway{keys: keys, quota: quota, cp: cp, log: log}
	for _, o := range opts {
		o(g)
	}
	return g
}

// GatewayOption configures a Gateway.
type GatewayOption func(*Gateway)

// WithTelemetry wires a product-telemetry emitter into the gateway. When the
// emitter is disabled (the default) the create path stays a no-op.
func WithTelemetry(t *telemetry.Emitter) GatewayOption {
	return func(g *Gateway) { g.tel = t }
}

// WithTrustedProxyHops sets the number of trusted reverse-proxy hops in front of
// the gateway for client-IP resolution. It returns the gateway for chaining. A
// negative value is clamped to zero (X-Forwarded-For not trusted). See
// TrustedProxyHops for the trust semantics.
func (g *Gateway) WithTrustedProxyHops(hops TrustedProxyHops) *Gateway {
	if hops < 0 {
		hops = 0
	}
	g.trustedHops = hops
	return g
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
	// Live fork (issues #596, #709): the flat SDK forks a running sandbox via
	// POST /v1/sandboxes/<id>/fork (DirectSandbox._fork_one). On the STANDALONE
	// server that path is a true live fork of the running source; on the hosted
	// gateway it now routes to the dedicated sandbox.fork op, whose control-plane
	// handler resolves the ORG-OWNED source from the path and submits a
	// Source.FromSandbox Sandbox (the live fork engine, #611), so hosted and
	// standalone agree: the child inherits the source's live memory and disk.
	// The flat POST /v1/fork below names NO source sandbox in its path and stays
	// a template claim (sandbox.create). Without this case the POST falls through
	// to "sandbox.post" and the control plane rejects it as an unknown operation.
	case strings.HasPrefix(p, "sandboxes/") && strings.HasSuffix(p, "/fork") && method == http.MethodPost:
		return "sandbox.fork"
	// Template operations: the SDK calls POST /v1/templates (ensure_template) before
	// forking, and GET /v1/templates (list_templates) to discover available pools.
	// Without these cases both fall through to "sandbox.<method>", which the control
	// plane does not handle, producing "unknown operation" and breaking SDK create().
	case p == "templates" && method == http.MethodPost:
		return "template.ensure"
	case p == "templates" && method == http.MethodGet:
		return "template.list"
	// Fork: the SDK calls POST /v1/fork (SandboxServer.fork, DirectSandbox._fork_one)
	// to start a sandbox from a template. Both POST /v1/fork and POST /v1/sandboxes
	// create a new sandbox, so both map to sandbox.create. The body shape differs:
	// /v1/fork sends {"template":"<name>","id":"<id>"} while /v1/sandboxes sends
	// {"pool":"<name>"} or {"image":"<name>"}. The create handler resolves all three.
	case p == "fork" && method == http.MethodPost:
		return "sandbox.create"
	// Pause and resume: the SDK calls POST /v1/pause and POST /v1/resume with the
	// body {"sandbox":"<id>"} (issue #601). Both are mutating lifecycle verbs, so
	// the requiredScopeFor default (the sandboxes scope) is correct. The control
	// plane proxies them to the per-sandbox runtime endpoint, which already serves
	// the routes (internal/daemon/lifecycle_api.go).
	case p == "pause" && method == http.MethodPost:
		return "sandbox.pause"
	case p == "resume" && method == http.MethodPost:
		return "sandbox.resume"
	default:
		return "sandbox." + strings.ToLower(method)
	}
}

// apiKeyUnauthorized is the gateway's 401 for a missing or invalid ORG api key.
// It reuses the generic unauthorized code but replaces the catalogue message and
// remediation, which describe the per-sandbox bearer token ("the
// <name>-sandbox-token Secret") and mislead a hosted SDK caller whose auth is an
// org api key, not a sandbox token (the #28 LLM-legible error rule). The caller
// adds the specific cause.
func apiKeyUnauthorized() apierr.Error {
	return apierr.Get(apierr.CodeUnauthorized).
		WithMessage("the request is not authenticated: missing or invalid api key").
		WithRemediation("Send Authorization: Bearer <your mitos api key> (it starts with mitos_live_); create or manage keys in the console.")
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
		g.metrics.observeAuthDenial(denialMissingKey)
		g.fail(w, apiKeyUnauthorized().
			WithCause("no bearer api key was presented"))
		return
	}

	res, err := g.keys.Verify(r.Context(), raw, scope)
	if err != nil {
		g.failVerify(w, err)
		return
	}
	orgID := res.Key.OrgID

	// Resolve the trusted client IP and stash it in the request context so the
	// quota enforcer can charge the per-IP rate-limit bucket against the real
	// source address. The IP is taken from the connection RemoteAddr unless a
	// trusted-proxy hop count is configured; a caller-set X-Forwarded-For can
	// never move the bucket off the real source (see TrustedProxyHops).
	ctx := withClientIP(r.Context(), clientIP(r, g.trustedHops))

	// Quota is enforced AFTER authn and org-resolution, BEFORE forwarding, so a
	// denied request never touches the control plane. The real enforcer (issue
	// #213) distinguishes a quota cap (quota_exceeded) from a rate-limit denial
	// (rate_limited) and a suspension (forbidden): when the enforcer error supplies
	// its own envelope via the APIError seam, the gateway honors it; otherwise it
	// falls back to the generic quota_exceeded envelope.
	if qErr := g.quota.Check(ctx, orgID, op); qErr != nil {
		g.log.Info("gateway quota denied", "key_id", res.Key.ID, "org", orgID, "op", op)
		g.fail(w, quotaEnvelope(qErr, op))
		return
	}

	// A runtime call that is a WebSocket upgrade (the interactive PTY rides the
	// Connect Exec RPC over a WebSocket) cannot go through the buffer-and-stream
	// Forward seam: it is a connection upgrade the gateway must hijack and pipe.
	// Auth, scope, and quota are already enforced above, so the org-scoped target
	// is resolved and the upgrade proxied here.
	if op == opRuntime && isWebSocketUpgrade(r) {
		g.proxyRuntimeWebSocket(ctx, w, r, orgID)
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

	// The Forward round trip is the server-side view of the latency the SDK
	// observes (create readiness wait, runtime proxy); it feeds the duration
	// histogram labeled by the bounded op and status class.
	forwardStarted := time.Now()
	resp, err := g.cp.Forward(ctx, fwd)
	if err != nil {
		e := apierr.Get(apierr.CodeInternal).
			WithCause("the control plane could not service the request")
		g.metrics.observeForwardDuration(op, e.Status, time.Since(forwardStarted))
		g.fail(w, e)
		return
	}
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	g.metrics.observeForwardDuration(op, status, time.Since(forwardStarted))

	// Product telemetry: a successful create emits a sandbox.created event and a
	// successful live fork (sandbox.fork) emits sandbox.forked, so feature
	// adoption is measurable per verb. This is a no-op when telemetry is disabled
	// (the default). The event carries ONLY non-PII properties (a success flag
	// and, when present, the non-identifying pool name the control plane echoes
	// via the X-Mitos-Pool response header); the org id is hashed by the emitter
	// and never sent raw. No body, image content, or customer payload is
	// inspected.
	if (op == "sandbox.create" || op == "sandbox.fork") && status >= 200 && status < 300 && g.tel.Enabled() {
		props := map[string]any{"success": true}
		if pool := resp.Header.Get("X-Mitos-Pool"); pool != "" {
			props["pool"] = pool
		}
		name := "sandbox.created"
		if op == "sandbox.fork" {
			name = "sandbox.forked"
		}
		g.tel.Emit(r.Context(), telemetry.Event{
			Name:       name,
			OrgID:      orgID,
			Properties: props,
		})
	}
	// A control-plane response may carry its own headers (the runtime proxy sets
	// Content-Type so a streamed Connect body is decodable). Default to JSON for
	// the lifecycle ops.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	g.metrics.observeStatus(status)
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
		g.metrics.observeAuthDenial(denialForbidden)
		g.fail(w, apierr.Get(apierr.CodeForbidden).
			WithCause("the key is valid but not permitted for this action"))
	default:
		// Malformed, unknown, expired, revoked all collapse to unauthorized so the
		// response does not reveal which one applies.
		g.metrics.observeAuthDenial(denialUnauthorized)
		g.fail(w, apiKeyUnauthorized().
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

// fail writes the error envelope. It never includes any secret value. Every
// error path funnels here, so this is the single request-outcome observation
// point for failures (the success write in ServeHTTP is the other).
func (g *Gateway) fail(w http.ResponseWriter, e apierr.Error) {
	g.metrics.observeStatus(e.Status)
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
