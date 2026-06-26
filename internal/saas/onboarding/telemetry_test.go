package onboarding

import (
	"context"
	"testing"
	"time"

	"mitos.run/mitos/internal/telemetry"
)

// waitFor polls fn until true or the deadline elapses.
func waitFor(t *testing.T, d time.Duration, fn func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return fn()
}

// TestTelemetryRecorderForwardsCuratedEvents: signup_started and verified are
// forwarded to product telemetry as signup.started and signup.verified, carrying
// no email or token; other funnel steps are not forwarded.
func TestTelemetryRecorderForwardsCuratedEvents(t *testing.T) {
	sink := telemetry.NewRecordingSink()
	tel := telemetry.New(telemetry.Config{Enabled: true, Sink: sink, Salt: "pepper", FlushInterval: 5 * time.Millisecond}, nil)
	defer func() { _ = tel.Shutdown(context.Background()) }()

	inner := NewMemEventRecorder()
	rec := NewTelemetryRecorder(inner, tel)

	rec.Record(context.Background(), Event{Subject: "subj-1", Name: EventSignupStarted, At: time.Now()})
	rec.Record(context.Background(), Event{Subject: "subj-1", Name: EventVerified, At: time.Now()})
	rec.Record(context.Background(), Event{Subject: "subj-1", Name: EventKeyIssued, At: time.Now()}) // not forwarded

	if !waitFor(t, time.Second, func() bool { return sink.Len() == 2 }) {
		t.Fatalf("expected 2 forwarded events, got %d", sink.Len())
	}
	names := map[string]bool{}
	for _, e := range sink.Events() {
		names[e.Name] = true
		// No raw org id is ever sent (the funnel event has none); org hash stays empty.
		if e.OrgHash != "" {
			t.Errorf("unexpected org hash on funnel-forwarded event: %q", e.OrgHash)
		}
	}
	if !names["signup.started"] || !names["signup.verified"] {
		t.Fatalf("missing curated events: %v", names)
	}
	if names["key_issued"] {
		t.Error("key_issued should not be forwarded to product telemetry")
	}

	// The inner recorder still has every event for funnel aggregation.
	if got := len(inner.Events(context.Background())); got != 3 {
		t.Fatalf("inner recorder has %d events, want 3", got)
	}
}

// TestTelemetryRecorderDisabledPassThrough: with telemetry disabled (the
// default), the recorder is a plain pass-through and emits nothing.
func TestTelemetryRecorderDisabledPassThrough(t *testing.T) {
	sink := telemetry.NewRecordingSink()
	tel := telemetry.New(telemetry.Config{Sink: sink, Salt: "pepper"}, nil) // disabled
	inner := NewMemEventRecorder()
	rec := NewTelemetryRecorder(inner, tel)

	rec.Record(context.Background(), Event{Subject: "subj-1", Name: EventSignupStarted, At: time.Now()})
	time.Sleep(20 * time.Millisecond)
	if sink.Len() != 0 {
		t.Fatalf("disabled telemetry emitted %d events, want 0", sink.Len())
	}
	if got := len(inner.Events(context.Background())); got != 1 {
		t.Fatalf("inner recorder has %d events, want 1", got)
	}
}
