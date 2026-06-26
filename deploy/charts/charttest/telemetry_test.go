package charttest

import (
	"strings"
	"testing"
)

// TestTelemetryAbsentByDefault asserts product telemetry renders NO env on the
// gateway or console by default: it is opt-in and off, so a default install ships
// a no-op emitter.
func TestTelemetryAbsentByDefault(t *testing.T) {
	out := render(t)
	for _, banned := range []string{
		"MITOS_TELEMETRY_ENABLED",
		"MITOS_TELEMETRY_ENDPOINT",
		"MITOS_TELEMETRY_SALT",
		"MITOS_TELEMETRY_TOKEN",
	} {
		if strings.Contains(out, banned) {
			t.Errorf("default render contains telemetry env %q; telemetry must be off by default", banned)
		}
	}
}

// TestTelemetryDisabledWithoutEndpoint asserts that enabling telemetry without an
// endpoint renders nothing (fail closed): the binary would run a no-op emitter.
func TestTelemetryDisabledWithoutEndpoint(t *testing.T) {
	out := render(t, "telemetry.enabled=true")
	if strings.Contains(out, "MITOS_TELEMETRY_ENABLED") {
		t.Error("telemetry.enabled=true with no endpoint still rendered telemetry env; must fail closed")
	}
}

// TestTelemetryRendersWhenEnabled asserts that with enabled+endpoint set, both
// the gateway and console get the enable + endpoint env, and that the salt and
// token are sourced via secretKeyRef ONLY (never as plaintext values).
func TestTelemetryRendersWhenEnabled(t *testing.T) {
	out := render(t,
		"telemetry.enabled=true",
		"telemetry.endpoint=http://collector.example/ingest",
		"telemetry.saltSecretRef.name=mitos-telemetry",
		"telemetry.tokenSecretRef.name=mitos-telemetry",
	)
	gateway := section(t, out, "kind: Deployment", "mitos-gateway")
	console := section(t, out, "kind: Deployment", "mitos-console")
	for _, doc := range []struct {
		name string
		body string
	}{{"gateway", gateway}, {"console", console}} {
		if !strings.Contains(doc.body, "MITOS_TELEMETRY_ENABLED") {
			t.Errorf("%s missing MITOS_TELEMETRY_ENABLED", doc.name)
		}
		if !strings.Contains(doc.body, "http://collector.example/ingest") {
			t.Errorf("%s missing telemetry endpoint", doc.name)
		}
		// The salt and token must be secretKeyRef-sourced, never inline values.
		if !strings.Contains(doc.body, "MITOS_TELEMETRY_SALT") || !strings.Contains(doc.body, "name: \"mitos-telemetry\"") {
			t.Errorf("%s missing salt secretKeyRef", doc.name)
		}
		if !strings.Contains(doc.body, "MITOS_TELEMETRY_TOKEN") {
			t.Errorf("%s missing token secretKeyRef", doc.name)
		}
		saltIdx := strings.Index(doc.body, "MITOS_TELEMETRY_SALT")
		if saltIdx >= 0 {
			after := doc.body[saltIdx:]
			// The salt env entry must use valueFrom/secretKeyRef, never a plaintext value.
			block := after
			if next := strings.Index(after[len("MITOS_TELEMETRY_SALT"):], "- name:"); next >= 0 {
				block = after[:next+len("MITOS_TELEMETRY_SALT")]
			}
			if !strings.Contains(block, "secretKeyRef") {
				t.Errorf("%s salt is not secretKeyRef-sourced:\n%s", doc.name, block)
			}
		}
	}
}

// TestTelemetryOptOutRenders asserts the per-deploy opt-out renders the
// force-disable env when telemetry is otherwise enabled.
func TestTelemetryOptOutRenders(t *testing.T) {
	out := render(t,
		"telemetry.enabled=true",
		"telemetry.endpoint=http://collector.example/ingest",
		"telemetry.optOut=true",
	)
	if !strings.Contains(out, "MITOS_TELEMETRY_OPTOUT") {
		t.Error("telemetry.optOut=true did not render MITOS_TELEMETRY_OPTOUT")
	}
}
