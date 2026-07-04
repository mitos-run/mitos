package billingprovider

import "github.com/prometheus/client_golang/prometheus"

// WebhookMetrics is the Prometheus view of the billing webhook endpoint
// (issue #617). Two counters, each backing one alert:
//
//   - verifyFailures: requests refused because the provider signature did not
//     verify (forged, replayed, malformed, or, the operationally urgent case, a
//     rotated/mismatched webhook secret rejecting EVERY legitimate provider
//     event, which silently stops top-ups and status syncs).
//   - handlerErrors: verified events answered 5xx (a store failure behind the
//     link, top-up, customer-lookup, or status write). The provider retries,
//     so a sustained rate means billing state is not landing.
//
// No label carries an org id, customer ref, transaction ref, or any payload
// detail; both are plain counters.
type WebhookMetrics struct {
	verifyFailures prometheus.Counter
	handlerErrors  prometheus.Counter
}

// NewWebhookMetrics builds the webhook counters. They are unregistered; the
// wiring (cmd/console) registers them onto its metrics registry with
// MustRegister, mirroring the internal/usage Metrics shape.
func NewWebhookMetrics() *WebhookMetrics {
	return &WebhookMetrics{
		verifyFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mitos_billing_webhook_verify_failures_total",
			Help: "Billing webhook requests refused because the provider signature did not verify (forged, replayed, malformed, or a rotated/mismatched webhook secret). Legitimate volume is zero; a sustained rate is always actionable.",
		}),
		handlerErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mitos_billing_webhook_errors_total",
			Help: "Signature-verified billing webhook events answered 5xx (customer link, top-up credit, customer lookup, or status write failed). The provider retries these; a sustained rate means top-ups and billing status changes are not landing.",
		}),
	}
}

// MustRegister registers the webhook counters on reg. It panics on a duplicate
// registration, the standard fail-fast for a misconfigured wiring.
func (m *WebhookMetrics) MustRegister(reg prometheus.Registerer) {
	reg.MustRegister(m.verifyFailures, m.handlerErrors)
}

// observeVerifyFailure counts one refused (unverified) webhook. Nil-safe so a
// handler constructed without metrics costs nothing.
func (m *WebhookMetrics) observeVerifyFailure() {
	if m == nil {
		return
	}
	m.verifyFailures.Inc()
}

// observeHandlerError counts one verified event answered 5xx. Nil-safe.
func (m *WebhookMetrics) observeHandlerError() {
	if m == nil {
		return
	}
	m.handlerErrors.Inc()
}

// WithMetrics wires a WebhookMetrics into the handler and returns the handler
// for chaining. A nil metrics keeps every observation a no-op.
func (h *WebhookHandler) WithMetrics(m *WebhookMetrics) *WebhookHandler {
	h.metrics = m
	return h
}
