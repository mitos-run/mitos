package telemetry

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingSink captures every batch it receives so a test can assert on what
// (if anything) the emitter delivered.
type recordingSink struct {
	mu     sync.Mutex
	events []sentEvent
	sends  int
	// block, when set, holds Send until released, to exercise the full-queue path.
	block   chan struct{}
	blocked chan struct{}
}

func (r *recordingSink) Send(_ context.Context, events []sentEvent) error {
	if r.block != nil {
		if r.blocked != nil {
			select {
			case r.blocked <- struct{}{}:
			default:
			}
		}
		<-r.block
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends++
	r.events = append(r.events, events...)
	return nil
}

func (r *recordingSink) all() []sentEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]sentEvent, len(r.events))
	copy(out, r.events)
	return out
}

func (r *recordingSink) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// waitFor polls fn until it returns true or the deadline elapses.
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

// TestDisabledByDefault: a zero-config emitter sends nothing and Emit is a no-op.
func TestDisabledByDefault(t *testing.T) {
	sink := &recordingSink{}
	e := New(Config{Sink: sink}, nil) // Enabled defaults false
	if e.Enabled() {
		t.Fatal("zero-config emitter reported enabled")
	}
	e.Emit(context.Background(), Event{Name: "sandbox.created", OrgID: "org-1"})
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if got := sink.count(); got != 0 {
		t.Fatalf("disabled emitter delivered %d events, want 0", got)
	}
}

// TestEnabledRequiresSink: Enabled with no sink stays disabled (fail closed).
func TestEnabledRequiresSink(t *testing.T) {
	e := New(Config{Enabled: true, Salt: "s"}, nil)
	if e.Enabled() {
		t.Fatal("enabled with no sink should fail closed to disabled")
	}
}

// TestDoNotTrackForcesDisabled: DO_NOT_TRACK overrides an otherwise-enabled config.
func TestDoNotTrackForcesDisabled(t *testing.T) {
	sink := &recordingSink{}
	e := New(Config{Enabled: true, Sink: sink, Salt: "s", DoNotTrack: true}, nil)
	if e.Enabled() {
		t.Fatal("DO_NOT_TRACK did not force-disable telemetry")
	}
	e.Emit(context.Background(), Event{Name: "sandbox.created", OrgID: "org-1"})
	_ = e.Shutdown(context.Background())
	if got := sink.count(); got != 0 {
		t.Fatalf("DO_NOT_TRACK emitter delivered %d events, want 0", got)
	}
}

// TestOptOutForcesDisabled: an explicit per-deploy opt-out disables telemetry.
func TestOptOutForcesDisabled(t *testing.T) {
	sink := &recordingSink{}
	e := New(Config{Enabled: true, Sink: sink, Salt: "s", OptOut: true}, nil)
	if e.Enabled() {
		t.Fatal("OptOut did not disable telemetry")
	}
	_ = e.Shutdown(context.Background())
	if got := sink.count(); got != 0 {
		t.Fatalf("opted-out emitter delivered %d events, want 0", got)
	}
}

// TestFromEnvDoNotTrack: DO_NOT_TRACK in the environment disables FromEnv even
// with a full enabled+endpoint config present.
func TestFromEnvDoNotTrack(t *testing.T) {
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvEndpoint, "http://collector.invalid/ingest")
	t.Setenv(EnvSalt, "pepper")
	t.Setenv(EnvDoNotTrack, "1")
	e := FromEnv(nil)
	if e.Enabled() {
		t.Fatal("FromEnv honored enabled config despite DO_NOT_TRACK=1")
	}
}

// TestFromEnvEnabled: a complete env opt-in produces an enabled emitter.
func TestFromEnvEnabled(t *testing.T) {
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvEndpoint, "http://collector.invalid/ingest")
	t.Setenv(EnvSalt, "pepper")
	t.Setenv(EnvDoNotTrack, "")
	t.Setenv(EnvOptOut, "")
	e := FromEnv(nil)
	if !e.Enabled() {
		t.Fatal("FromEnv with full opt-in config produced a disabled emitter")
	}
	_ = e.Shutdown(context.Background())
}

// TestFromEnvNoEndpointDisabled: opting in without an endpoint fails closed.
func TestFromEnvNoEndpointDisabled(t *testing.T) {
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvEndpoint, "")
	t.Setenv(EnvDoNotTrack, "")
	t.Setenv(EnvOptOut, "")
	e := FromEnv(nil)
	if e.Enabled() {
		t.Fatal("FromEnv enabled with no endpoint; must fail closed")
	}
}

// TestNoRawOrgID: an emitted event NEVER carries the raw org id, only its salted
// hash; the hash is stable and not equal to the raw id.
func TestNoRawOrgID(t *testing.T) {
	sink := &recordingSink{}
	e := New(Config{Enabled: true, Sink: sink, Salt: "pepper", FlushInterval: 5 * time.Millisecond}, nil)
	defer func() { _ = e.Shutdown(context.Background()) }()

	const rawOrg = "org-secret-123"
	e.Emit(context.Background(), Event{Name: "signup.verified", OrgID: rawOrg})
	if !waitFor(t, time.Second, func() bool { return sink.count() == 1 }) {
		t.Fatalf("event not flushed, got %d", sink.count())
	}
	got := sink.all()[0]
	if got.OrgHash == "" {
		t.Fatal("org hash is empty though a salt was configured")
	}
	if got.OrgHash == rawOrg {
		t.Fatal("org hash equals the raw org id")
	}
	if strings.Contains(got.OrgHash, rawOrg) {
		t.Fatal("org hash contains the raw org id")
	}
	// Stability: hashing the same id again yields the same hash.
	again := e.hashOrg(rawOrg)
	if again != got.OrgHash {
		t.Fatalf("org hash not stable: %q vs %q", again, got.OrgHash)
	}
}

// TestNoSaltDropsOrgID: with no salt configured the org id is dropped (fail
// closed), never sent raw.
func TestNoSaltDropsOrgID(t *testing.T) {
	sink := &recordingSink{}
	e := New(Config{Enabled: true, Sink: sink, Salt: "", FlushInterval: 5 * time.Millisecond}, nil)
	defer func() { _ = e.Shutdown(context.Background()) }()

	const rawOrg = "org-no-salt"
	e.Emit(context.Background(), Event{Name: "signup.started", OrgID: rawOrg})
	if !waitFor(t, time.Second, func() bool { return sink.count() == 1 }) {
		t.Fatalf("event not flushed, got %d", sink.count())
	}
	got := sink.all()[0]
	if got.OrgHash != "" {
		t.Fatalf("org id was sent (%q) despite no salt; must be dropped", got.OrgHash)
	}
}

// TestDropsHighRiskProperties: deny-listed property keys are stripped before send.
func TestDropsHighRiskProperties(t *testing.T) {
	sink := &recordingSink{}
	e := New(Config{Enabled: true, Sink: sink, Salt: "pepper", FlushInterval: 5 * time.Millisecond}, nil)
	defer func() { _ = e.Shutdown(context.Background()) }()

	e.Emit(context.Background(), Event{
		Name:  "sandbox.created",
		OrgID: "org-1",
		Properties: map[string]any{
			"tier":          "free",   // allowed
			"replicas":      3,        // allowed
			"email":         "a@b.co", // dropped: PII
			"client_ip":     "1.2.3.4",
			"api_token":     "tok_x",
			"secret_value":  "shh",
			"user_name":     "alice",
			"password":      "p",
			"x_auth_cookie": "c",
		},
	})
	if !waitFor(t, time.Second, func() bool { return sink.count() == 1 }) {
		t.Fatalf("event not flushed, got %d", sink.count())
	}
	props := sink.all()[0].Properties
	for _, banned := range []string{"email", "client_ip", "api_token", "secret_value", "user_name", "password", "x_auth_cookie"} {
		if _, ok := props[banned]; ok {
			t.Errorf("high-risk property %q was not dropped", banned)
		}
	}
	if props["tier"] != "free" {
		t.Errorf("allowed property tier dropped: %v", props["tier"])
	}
	if props["replicas"] != 3 {
		t.Errorf("allowed property replicas dropped: %v", props["replicas"])
	}
}

// TestBatchedFlush: multiple events arrive as a batch and flush on the interval.
func TestBatchedFlush(t *testing.T) {
	sink := &recordingSink{}
	e := New(Config{Enabled: true, Sink: sink, Salt: "pepper", FlushInterval: 5 * time.Millisecond, BatchMax: 100}, nil)
	defer func() { _ = e.Shutdown(context.Background()) }()

	for i := 0; i < 10; i++ {
		e.Emit(context.Background(), Event{Name: "sandbox.created", OrgID: "org-1"})
	}
	if !waitFor(t, time.Second, func() bool { return sink.count() == 10 }) {
		t.Fatalf("expected 10 events, got %d", sink.count())
	}
}

// TestShutdownFlushesBuffer: events queued just before Shutdown are delivered by
// the final flush, even with a long flush interval that would not otherwise fire.
func TestShutdownFlushesBuffer(t *testing.T) {
	sink := &recordingSink{}
	e := New(Config{Enabled: true, Sink: sink, Salt: "pepper", FlushInterval: time.Hour}, nil)

	for i := 0; i < 5; i++ {
		e.Emit(context.Background(), Event{Name: "sandbox.created", OrgID: "org-1"})
	}
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if got := sink.count(); got != 5 {
		t.Fatalf("shutdown flushed %d events, want 5", got)
	}
}

// TestFullQueueDropsWithCounter: when the bounded queue is full, Emit drops the
// overflow and increments the drop counter rather than blocking.
func TestFullQueueDropsWithCounter(t *testing.T) {
	// A blocking sink so the single in-flight batch holds the background loop and
	// the queue fills. QueueSize 2, BatchMax 1 so the loop pulls one event, blocks
	// in Send, and the queue (cap 2) saturates after two more enqueues.
	sink := &recordingSink{block: make(chan struct{}), blocked: make(chan struct{}, 1)}
	e := New(Config{
		Enabled:       true,
		Sink:          sink,
		Salt:          "pepper",
		FlushInterval: time.Hour,
		QueueSize:     2,
		BatchMax:      1,
	}, nil)

	// First event is pulled by the loop and blocks in Send.
	e.Emit(context.Background(), Event{Name: "sandbox.created", OrgID: "org-1"})
	<-sink.blocked // the loop is now blocked inside Send

	// Fill the queue (cap 2), then overflow it. The overflow events must drop.
	for i := 0; i < 10; i++ {
		e.Emit(context.Background(), Event{Name: "sandbox.created", OrgID: "org-1"})
	}
	if e.Dropped() == 0 {
		t.Fatal("expected dropped events when the queue is full, got 0")
	}

	// Release the sink and shut down cleanly.
	close(sink.block)
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// TestDisabledEmitDoesNotPanicOnNil: a nil emitter is a safe no-op.
func TestDisabledEmitDoesNotPanicOnNil(t *testing.T) {
	var e *Emitter
	e.Emit(context.Background(), Event{Name: "x"})
	if e.Enabled() {
		t.Fatal("nil emitter reported enabled")
	}
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("nil shutdown: %v", err)
	}
}

// TestTimestampStamped: an event with a zero timestamp gets stamped at Emit.
func TestTimestampStamped(t *testing.T) {
	sink := &recordingSink{}
	fixed := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	e := New(Config{Enabled: true, Sink: sink, Salt: "pepper", FlushInterval: 5 * time.Millisecond, Now: func() time.Time { return fixed }}, nil)
	defer func() { _ = e.Shutdown(context.Background()) }()

	e.Emit(context.Background(), Event{Name: "sandbox.created", OrgID: "org-1"})
	if !waitFor(t, time.Second, func() bool { return sink.count() == 1 }) {
		t.Fatalf("event not flushed")
	}
	if !sink.all()[0].Timestamp.Equal(fixed) {
		t.Fatalf("timestamp not stamped: got %v", sink.all()[0].Timestamp)
	}
}
