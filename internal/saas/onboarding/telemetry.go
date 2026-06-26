package onboarding

import (
	"context"

	"mitos.run/mitos/internal/telemetry"
)

// telemetryEventNames maps the internal funnel event names to the stable product
// -telemetry event names. Only this curated subset is forwarded to product
// telemetry; the remaining funnel steps stay internal-only (they are aggregated
// for the funnel-stats dashboard, not the product-analytics pipeline). The
// mapped events carry NO email and NO token (the funnel Event already excludes
// them by construction); the subject is a non-PII opaque id.
var telemetryEventNames = map[EventName]string{
	EventSignupStarted: "signup.started",
	EventVerified:      "signup.verified",
}

// TelemetryRecorder is an EventRecorder that forwards the curated signup funnel
// events to a product-telemetry Emitter while delegating to an inner recorder
// for the full funnel aggregation. It aligns with the existing EventRecorder seam
// (internal/saas/onboarding) rather than duplicating a second hook.
//
// Privacy: telemetry is opt-in and off by default, so when the emitter is
// disabled this is a cheap pass-through to the inner recorder. The forwarded
// event carries no email or token; the funnel subject is an opaque id passed as a
// non-PII property. The org id, when known, is hashed by the emitter (the
// recorder never sends a raw org id).
type TelemetryRecorder struct {
	inner EventRecorder
	tel   *telemetry.Emitter
}

// NewTelemetryRecorder wraps inner so the curated signup events are also sent to
// tel. A nil inner defaults to a discarding recorder; a nil or disabled emitter
// makes this a plain pass-through.
func NewTelemetryRecorder(inner EventRecorder, tel *telemetry.Emitter) *TelemetryRecorder {
	if inner == nil {
		inner = nopRecorder{}
	}
	return &TelemetryRecorder{inner: inner, tel: tel}
}

// Record delegates to the inner recorder and, for the curated subset, also emits
// a product-telemetry event. The org id is not available on the funnel Event, so
// it is left empty here; the emitter would hash it if present. The opaque subject
// is attached as a non-PII property so the product pipeline can dedupe a signup
// without any account identifier.
func (r *TelemetryRecorder) Record(ctx context.Context, e Event) {
	r.inner.Record(ctx, e)
	if r.tel == nil || !r.tel.Enabled() {
		return
	}
	name, ok := telemetryEventNames[e.Name]
	if !ok {
		return
	}
	r.tel.Emit(ctx, telemetry.Event{
		Name:       name,
		Properties: map[string]any{"funnel_subject": e.Subject},
		Timestamp:  e.At,
	})
}

// Events delegates to the inner recorder so funnel aggregation still works.
func (r *TelemetryRecorder) Events(ctx context.Context) []Event {
	return r.inner.Events(ctx)
}
