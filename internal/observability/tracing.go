// Package observability wires OpenTelemetry tracing for the control plane.
//
// Tracing is OFF by default: with no OTLP endpoint configured, Setup installs
// nothing and the global TracerProvider stays the OTel default no-op, so every
// span the controller and forkd start is a zero-cost no-op. A real provider
// (OTLP gRPC exporter) is installed only when an endpoint is supplied.
//
// Spans NEVER carry secret values. Only ids, counts, names, and timings are
// recorded as attributes; env, secrets, and tokens are excluded by
// construction at every call site.
package observability

import (
	"context"
	"fmt"
	"sync"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/stats"
)

// Setup builds and installs an OTel TracerProvider for the given service.
//
// When otlpEndpoint is non-empty, it builds an OTLP gRPC exporter pointed at
// that endpoint (host:port, insecure transport; the control plane runs the
// collector over the cluster network or a sidecar) and installs a batching
// provider as the global TracerProvider, plus the W3C trace-context propagator
// so trace ids cross the controller -> forkd gRPC boundary.
//
// When otlpEndpoint is empty, Setup installs NOTHING: the global provider stays
// the OTel default no-op so tracing is zero cost when disabled. The returned
// shutdown func is then a no-op.
//
// The returned shutdown func flushes and stops the exporter; callers defer it.
func Setup(ctx context.Context, serviceName, otlpEndpoint string) (shutdown func(context.Context) error, err error) {
	if otlpEndpoint == "" {
		// Tracing disabled: leave the global no-op provider in place.
		return func(context.Context) error { return nil }, nil
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(otlpEndpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("create otlp exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return tp.Shutdown, nil
}

// Tracer returns the named tracer from the global provider. When tracing is
// disabled (Setup not called with an endpoint), this is the default no-op
// tracer, so spans it starts cost nothing.
func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}

// inMemoryProvider is the process-wide test recorder and its provider. OTel's
// global delegating tracer wires its delegate exactly once per process (a
// sync.Once in the otel global state), so the FIRST in-memory provider installed
// is the one every cached package-level tracer forwards to for the rest of the
// process. A second, distinct recorder would never see those spans. Sharing one
// process-wide recorder across all InMemoryForTest callers keeps every test's
// spans flowing to the recorder the cached tracers are actually bound to.
var (
	inMemoryOnce     sync.Once
	inMemoryRecorder *inMemorySpanRecorder
)

// inMemorySpanRecorder is a tracetest.SpanRecorder whose ended-span buffer can be
// reset between tests, so each test that installs the shared recorder starts from
// a clean slate while keeping the single process-wide delegate binding intact.
type inMemorySpanRecorder struct {
	*tracetest.SpanRecorder
	tp *sdktrace.TracerProvider
}

// InMemoryForTest installs (once per process) an in-memory span recorder as the
// global TracerProvider and the W3C trace-context propagator, so tests can drive
// code that starts spans and then assert on what was recorded. It returns the
// shared recorder, reset to empty, and a restore func.
//
// The provider is installed only once because OTel delegates each cached tracer
// to the first real provider permanently; the restore func therefore does NOT
// swap the provider back (that would orphan the cached tracers on a no-op that
// never receives spans). Instead it resets the recorder buffer so the next test
// starts clean. Each test anchors its assertions on its own uniquely named
// objects, so the shared, accumulating recorder is unambiguous.
//
// Cross-process propagation tests rely on the propagator being set so a gRPC
// client injects trace context and the server-side recorder shares the trace id.
func InMemoryForTest() (*tracetest.SpanRecorder, func()) {
	inMemoryOnce.Do(func() {
		recorder := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(propagation.TraceContext{})
		inMemoryRecorder = &inMemorySpanRecorder{SpanRecorder: recorder, tp: tp}
	})

	// Reset the shared recorder so this test sees only spans it produces from now
	// on. tracetest.SpanRecorder has no public reset, so we rebind a fresh
	// recorder as the provider's processor: the provider is the SAME object the
	// cached tracers already delegate to, so the new processor takes effect
	// without re-triggering the one-time delegate.
	fresh := tracetest.NewSpanRecorder()
	inMemoryRecorder.tp.UnregisterSpanProcessor(inMemoryRecorder.SpanRecorder)
	inMemoryRecorder.tp.RegisterSpanProcessor(fresh)
	inMemoryRecorder.SpanRecorder = fresh
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return fresh, func() {
		// Leave the shared provider in place (the cached tracers are bound to it);
		// just detach this test's recorder so later spans are not appended to a
		// buffer a finished test still holds.
		inMemoryRecorder.tp.UnregisterSpanProcessor(fresh)
	}
}

// GRPCClientStatsHandler returns the otelgrpc client stats handler used on the
// controller's dial to forkd, so trace context propagates over the wire.
func GRPCClientStatsHandler() stats.Handler {
	return otelgrpc.NewClientHandler()
}

// GRPCServerStatsHandler returns the otelgrpc server stats handler installed on
// forkd's gRPC server, the receiving half of context propagation.
func GRPCServerStatsHandler() stats.Handler {
	return otelgrpc.NewServerHandler()
}
