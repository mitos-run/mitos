package saas

import (
	"context"
	"net/http"
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

// TestGatewayEmitsSandboxCreatedWhenEnabled: a successful create emits a
// sandbox.created product event carrying the SALTED org hash (never the raw org
// id) and only non-PII properties.
func TestGatewayEmitsSandboxCreatedWhenEnabled(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	sink := telemetry.NewRecordingSink()
	tel := telemetry.New(telemetry.Config{
		Enabled:       true,
		Sink:          sink,
		Salt:          "pepper",
		SinkName:      "recording",
		FlushInterval: 5 * time.Millisecond,
	}, nil)
	defer func() { _ = tel.Shutdown(context.Background()) }()

	// Rebuild the gateway with telemetry wired and a create response carrying a
	// non-identifying pool header the gateway forwards as a property.
	fx.cp.respHeader = http.Header{"X-Mitos-Pool": []string{"default-pool"}}
	gw := NewGateway(fx.keys, nil, fx.cp, nil, WithTelemetry(tel))

	rec := doRequest(gw, http.MethodPost, "/v1/sandboxes", fx.rawA, `{"pool":"default-pool"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200", rec.Code)
	}
	if !waitFor(t, time.Second, func() bool { return sink.Len() == 1 }) {
		t.Fatalf("expected 1 telemetry event, got %d", sink.Len())
	}
	ev := sink.Events()[0]
	if ev.Name != "sandbox.created" {
		t.Fatalf("event name = %q, want sandbox.created", ev.Name)
	}
	if ev.OrgHash == "" || ev.OrgHash == fx.orgA {
		t.Fatalf("org hash missing or equals raw org id: %q", ev.OrgHash)
	}
	if ev.Properties["pool"] != "default-pool" {
		t.Errorf("pool property = %v, want default-pool", ev.Properties["pool"])
	}
	if ev.Properties["success"] != true {
		t.Errorf("success property = %v, want true", ev.Properties["success"])
	}
}

// TestGatewayNoTelemetryWhenDisabled: with a disabled emitter (the default), a
// create emits nothing.
func TestGatewayNoTelemetryWhenDisabled(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	sink := telemetry.NewRecordingSink()
	// Disabled: Enabled defaults false, so this is a no-op emitter.
	tel := telemetry.New(telemetry.Config{Sink: sink, Salt: "pepper"}, nil)
	gw := NewGateway(fx.keys, nil, fx.cp, nil, WithTelemetry(tel))

	rec := doRequest(gw, http.MethodPost, "/v1/sandboxes", fx.rawA, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200", rec.Code)
	}
	// Give any (erroneous) async send a moment.
	time.Sleep(20 * time.Millisecond)
	if sink.Len() != 0 {
		t.Fatalf("disabled telemetry emitted %d events, want 0", sink.Len())
	}
}

// TestGatewayNoTelemetryOnNonCreate: a list (read) op never emits sandbox.created.
func TestGatewayNoTelemetryOnNonCreate(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	sink := telemetry.NewRecordingSink()
	tel := telemetry.New(telemetry.Config{Enabled: true, Sink: sink, Salt: "pepper", FlushInterval: 5 * time.Millisecond}, nil)
	defer func() { _ = tel.Shutdown(context.Background()) }()
	gw := NewGateway(fx.keys, nil, fx.cp, nil, WithTelemetry(tel))

	rec := doRequest(gw, http.MethodGet, "/v1/sandboxes", fx.rawA, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", rec.Code)
	}
	time.Sleep(20 * time.Millisecond)
	if sink.Len() != 0 {
		t.Fatalf("list emitted %d telemetry events, want 0", sink.Len())
	}
}
