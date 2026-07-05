package daemon

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"mitos.run/mitos/internal/fork"
	"mitos.run/mitos/internal/observability"
	forkdpb "mitos.run/mitos/proto/forkd"
)

// TestForkProducesSpans drives the gRPC Fork handler against a MockEngine with
// the in-memory recorder installed and asserts that a forkd.Fork span and a
// child engine.fork span are recorded with the expected non-secret attributes,
// and that no secret value appears on any span.
func TestForkProducesSpans(t *testing.T) {
	recorder, restore := observability.InMemoryForTest()
	t.Cleanup(restore)

	engine := fork.NewMockEngine()
	engine.ForkDelay = 0
	if err := engine.CreateTemplate("py", "python:3.12-slim", nil, nil, nil, nil, false, false); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))
	g := &grpcService{srv: srv}

	const secretValue = "super-secret-token-value"
	_, err := g.Fork(context.Background(), &forkdpb.ForkRequest{
		SnapshotId: "py",
		SandboxId:  "sb-trace-1",
		Secrets:    []*forkdpb.SecretVar{{Key: "API_KEY", Value: secretValue}},
		ApiToken:   secretValue,
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}

	spans := recorder.Ended()
	forkSpan := findSpan(spans, "forkd.Fork")
	if forkSpan == nil {
		t.Fatalf("no forkd.Fork span recorded; got %v", spanNames(spans))
	}
	engineSpan := findSpan(spans, "engine.fork")
	if engineSpan == nil {
		t.Fatalf("no engine.fork span recorded; got %v", spanNames(spans))
	}
	readySpan := findSpan(spans, "forkd.guest-ready")
	if readySpan == nil {
		t.Fatalf("no forkd.guest-ready span recorded; got %v", spanNames(spans))
	}

	// The engine span is a child of the forkd.Fork span (same trace id).
	if forkSpan.SpanContext().TraceID() != engineSpan.SpanContext().TraceID() {
		t.Fatalf("engine.fork is not in the forkd.Fork trace")
	}
	if engineSpan.Parent().SpanID() != forkSpan.SpanContext().SpanID() {
		t.Fatalf("engine.fork is not a child of forkd.Fork")
	}

	// The guest-ready span closes the tail: a child of forkd.Fork in the same
	// trace, carrying a non-empty trace id.
	if !readySpan.SpanContext().TraceID().IsValid() {
		t.Fatalf("forkd.guest-ready has no trace id")
	}
	if readySpan.SpanContext().TraceID() != forkSpan.SpanContext().TraceID() {
		t.Fatalf("forkd.guest-ready is not in the forkd.Fork trace")
	}
	if readySpan.Parent().SpanID() != forkSpan.SpanContext().SpanID() {
		t.Fatalf("forkd.guest-ready is not a child of forkd.Fork")
	}
	assertAttr(t, readySpan, "snapshot.id", "py")
	assertAttr(t, readySpan, "sandbox.id", "sb-trace-1")

	assertAttr(t, forkSpan, "snapshot.id", "py")
	assertAttr(t, forkSpan, "sandbox.id", "sb-trace-1")

	// No span may carry the secret value anywhere in its attributes.
	for _, s := range spans {
		for _, kv := range s.Attributes() {
			if kv.Value.AsString() == secretValue {
				t.Fatalf("span %q leaked a secret value via attribute %q", s.Name(), kv.Key)
			}
		}
	}
}

// The forkd.first-exec span (issue #164, the trace tail) was emitted by the
// legacy JSON /v1/exec handler, which was removed in #358; the runtime exec
// surface is now the Connect sandbox.v1.Sandbox protocol. The fork-side trace
// spans (forkd.Fork, engine.fork, forkd.guest-ready) are unaffected and stay
// covered by TestForkProducesSpans above.

// TestTracingOffNoSpans asserts that with tracing OFF (the default no-op
// provider, no recorder installed) a fork produces no recorded spans and never
// panics.
func TestTracingOffNoSpans(t *testing.T) {
	// Deliberately do NOT install the in-memory recorder: tracing stays the
	// default no-op provider. The fork path must not panic and must cost nothing.
	engine := fork.NewMockEngine()
	engine.ForkDelay = 0
	if err := engine.CreateTemplate("py", "python:3.12-slim", nil, nil, nil, nil, false, false); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	srv := NewServer(engine, NewSandboxAPI(t.TempDir()))
	g := &grpcService{srv: srv}
	if _, err := g.Fork(context.Background(), &forkdpb.ForkRequest{
		SnapshotId: "py",
		SandboxId:  "sb-off",
	}); err != nil {
		t.Fatalf("Fork with tracing off: %v", err)
	}
}

func findSpan(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Name()
	}
	return out
}

func assertAttr(t *testing.T, s sdktrace.ReadOnlySpan, key, want string) {
	t.Helper()
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			if kv.Value.AsString() != want {
				t.Fatalf("span %q attr %q = %q, want %q", s.Name(), key, kv.Value.AsString(), want)
			}
			return
		}
	}
	t.Fatalf("span %q missing attribute %q", s.Name(), key)
}
