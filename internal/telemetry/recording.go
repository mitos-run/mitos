package telemetry

import (
	"context"
	"sync"
)

// RecordedEvent is a snapshot of one event as it crossed the Sink boundary,
// exposed so OTHER packages can assert on what their wiring emitted WITHOUT
// reaching the unexported on-wire type. It deliberately exposes OrgHash (never a
// raw org id) and the sanitized Properties, so a wiring test can prove the
// no-PII guarantee end to end.
type RecordedEvent struct {
	Name       string
	Properties map[string]any
	// OrgHash is the salted hash of the org id, or "" when no salt was configured
	// (the org id is dropped). A raw org id is never present.
	OrgHash string
}

// RecordingSink is an in-memory Sink that captures sanitized events for wiring
// tests in other packages. It is exported because the on-wire event type is
// unexported; a caller in another package cannot otherwise implement Sink.
type RecordingSink struct {
	mu       sync.Mutex
	recorded []RecordedEvent
}

// NewRecordingSink returns an empty RecordingSink.
func NewRecordingSink() *RecordingSink { return &RecordingSink{} }

// Send captures the batch.
func (r *RecordingSink) Send(_ context.Context, events []sentEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range events {
		r.recorded = append(r.recorded, RecordedEvent{
			Name:       events[i].Name,
			Properties: events[i].Properties,
			OrgHash:    events[i].OrgHash,
		})
	}
	return nil
}

// Events returns a copy of the captured events in arrival order.
func (r *RecordingSink) Events() []RecordedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RecordedEvent, len(r.recorded))
	copy(out, r.recorded)
	return out
}

// Len returns how many events have been captured.
func (r *RecordingSink) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.recorded)
}
