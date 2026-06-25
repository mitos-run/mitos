package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
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
	if err := engine.CreateTemplate("py", "python:3.12-slim", nil, nil); err != nil {
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

// TestFirstExecProducesSpan drives two execs against a fake guest and asserts
// that only the FIRST gets the forkd.first-exec span (with a first marker and an
// id-only attribute set), and that a second exec is NOT marked first.
func TestFirstExecProducesSpan(t *testing.T) {
	recorder, restore := observability.InMemoryForTest()
	t.Cleanup(restore)

	dir := shortVsockDir(t)
	sock := filepath.Join(dir, "sb-exec", "vsock.sock")
	startFakeGuestGRPCUDS(t, sock, &fakeGuestSandbox{execStdout: "hi\n", execExit: 0})
	api := NewSandboxAPI(dir)
	api.AllowTokenless()
	if err := api.RegisterSandbox("sb-exec", sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sb-exec", sock)

	httpSrv := httptest.NewServer(api.Handler())
	defer httpSrv.Close()

	const secretArg = "do-not-leak-this-command-arg"
	postExec := func() {
		body, _ := json.Marshal(map[string]any{"sandbox": "sb-exec", "command": "echo " + secretArg})
		resp, err := http.Post(httpSrv.URL+"/v1/exec", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	postExec()
	postExec()

	firstSpans := spansNamed(recorder.Ended(), "forkd.first-exec")
	if len(firstSpans) != 1 {
		t.Fatalf("want exactly one forkd.first-exec span (the second exec must not be marked first); got %d", len(firstSpans))
	}
	fs := firstSpans[0]
	if !fs.SpanContext().TraceID().IsValid() {
		t.Fatalf("forkd.first-exec has no trace id")
	}
	assertAttr(t, fs, "sandbox.id", "sb-exec")
	assertBoolAttr(t, fs, "first", true)

	// The span attributes carry only ids/booleans, never the command or its args.
	for _, s := range recorder.Ended() {
		for _, kv := range s.Attributes() {
			if v := kv.Value.AsString(); v == "echo "+secretArg || v == secretArg {
				t.Fatalf("span %q leaked the command via attribute %q", s.Name(), kv.Key)
			}
		}
	}
}

// TestTracingOffNoSpans asserts that with tracing OFF (the default no-op
// provider, no recorder installed) a fork plus an exec produce no recorded spans
// and never panic.
func TestTracingOffNoSpans(t *testing.T) {
	// Deliberately do NOT install the in-memory recorder: tracing stays the
	// default no-op provider. The fork+exec path must not panic and must cost
	// nothing.
	engine := fork.NewMockEngine()
	engine.ForkDelay = 0
	if err := engine.CreateTemplate("py", "python:3.12-slim", nil, nil); err != nil {
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
	// startFirstExecSpan must not panic with the no-op tracer; it returns a no-op
	// span whose End is also a no-op.
	api := NewSandboxAPI(t.TempDir())
	_, span := api.startFirstExecSpan(context.Background(), "sb-off")
	if span != nil {
		span.End()
	}
}

// TestFirstExecContinuesTrace asserts that when the exec request carries a W3C
// traceparent header, the forkd.first-exec span CONTINUES that trace (shares its
// trace id) rather than starting a fresh root.
func TestFirstExecContinuesTrace(t *testing.T) {
	recorder, restore := observability.InMemoryForTest()
	t.Cleanup(restore)

	dir := shortVsockDir(t)
	sock := filepath.Join(dir, "sb-cont", "vsock.sock")
	startFakeGuestGRPCUDS(t, sock, &fakeGuestSandbox{execStdout: "hi\n", execExit: 0})
	api := NewSandboxAPI(dir)
	api.AllowTokenless()
	if err := api.RegisterSandbox("sb-cont", sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sb-cont", sock)

	httpSrv := httptest.NewServer(api.Handler())
	defer httpSrv.Close()

	// Build a parent span context and inject its W3C traceparent into the request
	// headers, mimicking an SDK or controller that propagated a trace.
	tid, _ := trace.TraceIDFromHex("00000000000000000000000000000abc")
	sid, _ := trace.SpanIDFromHex("0000000000000abc")
	parent := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.FlagsSampled,
	})
	carrier := propagation.HeaderCarrier{}
	otel.GetTextMapPropagator().Inject(trace.ContextWithSpanContext(context.Background(), parent), carrier)

	body, _ := json.Marshal(map[string]any{"sandbox": "sb-cont", "command": "echo hi"})
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/v1/exec", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, k := range carrier.Keys() {
		req.Header.Set(k, carrier.Get(k))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	firstSpans := spansNamed(recorder.Ended(), "forkd.first-exec")
	if len(firstSpans) != 1 {
		t.Fatalf("want one forkd.first-exec span; got %d", len(firstSpans))
	}
	if firstSpans[0].SpanContext().TraceID() != tid {
		t.Fatalf("forkd.first-exec did not continue the propagated trace: got %s, want %s",
			firstSpans[0].SpanContext().TraceID(), tid)
	}
}

func spansNamed(spans []sdktrace.ReadOnlySpan, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

func assertBoolAttr(t *testing.T, s sdktrace.ReadOnlySpan, key string, want bool) {
	t.Helper()
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			if kv.Value.AsBool() != want {
				t.Fatalf("span %q attr %q = %v, want %v", s.Name(), key, kv.Value.AsBool(), want)
			}
			return
		}
	}
	t.Fatalf("span %q missing attribute %q", s.Name(), key)
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
