package saas

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// GatewayMetrics is the Prometheus view of the gateway's public front door
// (issue #617): the request outcome classes the GatewayErrorRateHigh alert
// rates, and the authentication denials the GatewayAuthDenialSpike alert
// watches for credential scanning or a broken integration.
//
// CARDINALITY + SECRET HYGIENE: the labels are a bounded status class
// (1xx..5xx) and a bounded denial reason (missing_key, unauthorized,
// forbidden). No key id, key prefix, org id, path, or any secret ever enters a
// label or value; the per-request log line carries the (non-secret) ids
// instead. Quota denials are deliberately NOT auth denials: an org hitting its
// cap must not page the auth alert.
type GatewayMetrics struct {
	requests    *prometheus.CounterVec
	authDenials *prometheus.CounterVec
}

// Auth denial reasons. Bounded set: the metric label values are only ever one
// of these.
const (
	// denialMissingKey is a request that presented no bearer key at all.
	denialMissingKey = "missing_key"
	// denialUnauthorized is a key that is malformed, unknown, expired, or
	// revoked (collapsed, like the public 401, so the metric cannot be used to
	// probe which one applies).
	denialUnauthorized = "unauthorized"
	// denialForbidden is a valid key not permitted for the action (scope or
	// wrong org).
	denialForbidden = "forbidden"
)

// NewGatewayMetrics builds the gateway metric vectors. They are unregistered;
// the wiring (cmd/gateway) registers them onto its metrics registry with
// MustRegister, mirroring the internal/usage Metrics shape.
func NewGatewayMetrics() *GatewayMetrics {
	return &GatewayMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mitos_gateway_requests_total",
			Help: "Public gateway requests by HTTP status class (1xx..5xx). A completed WebSocket runtime upgrade counts as 1xx (101 Switching Protocols). Labels carry only the bounded class, never a path, org, or key identifier.",
		}, []string{"code_class"}),
		authDenials: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mitos_gateway_auth_denials_total",
			Help: "Requests denied at authentication by reason: missing_key (no bearer presented), unauthorized (malformed, unknown, expired, or revoked key, collapsed like the public 401), forbidden (valid key, disallowed scope or org). Quota and rate-limit denials are excluded.",
		}, []string{"reason"}),
	}
}

// MustRegister registers the gateway metric vectors on reg. It panics on a
// duplicate registration, the standard fail-fast for a misconfigured wiring.
func (m *GatewayMetrics) MustRegister(reg prometheus.Registerer) {
	reg.MustRegister(m.requests, m.authDenials)
}

// observeStatus counts one completed request in its status class. Nil-safe so
// a gateway constructed without metrics (tests, older wiring) costs nothing.
func (m *GatewayMetrics) observeStatus(status int) {
	if m == nil {
		return
	}
	m.requests.WithLabelValues(codeClass(status)).Inc()
}

// observeAuthDenial counts one authentication denial. Nil-safe.
func (m *GatewayMetrics) observeAuthDenial(reason string) {
	if m == nil {
		return
	}
	m.authDenials.WithLabelValues(reason).Inc()
}

// codeClass buckets an HTTP status into its class label ("2xx", "5xx", ...).
// An out-of-range status (a misbehaving upstream) collapses to the single
// "other" label so a garbage status can never mint unbounded series.
func codeClass(status int) string {
	if status >= 100 && status < 600 {
		return strconv.Itoa(status/100) + "xx"
	}
	return "other"
}

// WithGatewayMetrics wires a GatewayMetrics into the gateway. When absent (the
// default) every observation is a no-op.
func WithGatewayMetrics(m *GatewayMetrics) GatewayOption {
	return func(g *Gateway) { g.metrics = m }
}
