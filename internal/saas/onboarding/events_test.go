package onboarding

import (
	"context"
	"testing"
	"time"
)

func base() time.Time { return time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC) }

// ev is a small helper to build an event at an offset from base.
func ev(subject string, name EventName, offset time.Duration) Event {
	return Event{Subject: subject, Name: name, At: base().Add(offset)}
}

func TestAggregateFunnelConversionAndTiming(t *testing.T) {
	// Two subjects start; one completes the whole funnel, one drops after verify.
	events := []Event{
		// Subject a: full funnel, first_exec 90s after signup_started.
		ev("a", EventSignupStarted, 0),
		ev("a", EventVerified, 10*time.Second),
		ev("a", EventKeyIssued, 20*time.Second),
		ev("a", EventFirstSandboxCreated, 60*time.Second),
		ev("a", EventFirstExec, 90*time.Second),
		// Subject b: starts and verifies, then drops.
		ev("b", EventSignupStarted, 0),
		ev("b", EventVerified, 30*time.Second),
	}

	stats := AggregateFunnel(events)

	if stats.SignupStarted != 2 {
		t.Fatalf("SignupStarted = %d, want 2", stats.SignupStarted)
	}
	if stats.FirstExec != 1 {
		t.Fatalf("FirstExec = %d, want 1", stats.FirstExec)
	}
	if got := stats.OverallConversionRate(); got != 0.5 {
		t.Fatalf("overall conversion = %v, want 0.5", got)
	}
	if stats.MedianTimeToFirstExec != 90*time.Second {
		t.Fatalf("median TTFE = %v, want 90s", stats.MedianTimeToFirstExec)
	}

	// First transition signup_started -> verified: both reached, both converted.
	if len(stats.Steps) != len(FunnelOrder)-1 {
		t.Fatalf("steps = %d, want %d", len(stats.Steps), len(FunnelOrder)-1)
	}
	s0 := stats.Steps[0]
	if s0.From != EventSignupStarted || s0.To != EventVerified {
		t.Fatalf("step 0 spans %s->%s", s0.From, s0.To)
	}
	if s0.Reached != 2 || s0.Converted != 2 {
		t.Fatalf("step 0 reached/converted = %d/%d, want 2/2", s0.Reached, s0.Converted)
	}
	if s0.ConversionRate() != 1.0 {
		t.Fatalf("step 0 conversion = %v, want 1.0", s0.ConversionRate())
	}
	// verified -> key_issued: 2 reached, only a converted.
	s1 := stats.Steps[1]
	if s1.Reached != 2 || s1.Converted != 1 {
		t.Fatalf("step 1 reached/converted = %d/%d, want 2/1", s1.Reached, s1.Converted)
	}
	if s1.ConversionRate() != 0.5 {
		t.Fatalf("step 1 conversion = %v, want 0.5", s1.ConversionRate())
	}
}

func TestAggregateFunnelEmpty(t *testing.T) {
	stats := AggregateFunnel(nil)
	if stats.SignupStarted != 0 || stats.FirstExec != 0 {
		t.Fatal("empty funnel must be all zero")
	}
	if stats.OverallConversionRate() != 0 {
		t.Fatal("empty overall conversion must be 0")
	}
	if stats.MedianTimeToFirstExec != 0 {
		t.Fatal("empty median must be 0")
	}
}

func TestAggregateFunnelDedupesReplayedEventByEarliest(t *testing.T) {
	// A replayed signup_started at a later time must not skew the timing: the
	// earliest timestamp wins.
	events := []Event{
		ev("a", EventSignupStarted, 0),
		ev("a", EventSignupStarted, 100*time.Second), // replay, should be ignored
		ev("a", EventVerified, 10*time.Second),
		ev("a", EventKeyIssued, 20*time.Second),
		ev("a", EventFirstSandboxCreated, 30*time.Second),
		ev("a", EventFirstExec, 40*time.Second),
	}
	stats := AggregateFunnel(events)
	if stats.MedianTimeToFirstExec != 40*time.Second {
		t.Fatalf("median TTFE = %v, want 40s (earliest signup wins)", stats.MedianTimeToFirstExec)
	}
}

func TestMemEventRecorderRoundtrip(t *testing.T) {
	ctx := context.Background()
	r := NewMemEventRecorder()
	r.Record(ctx, ev("a", EventSignupStarted, 0))
	r.Record(ctx, ev("a", EventVerified, time.Second))
	got := r.Events(ctx)
	if len(got) != 2 {
		t.Fatalf("recorded %d events, want 2", len(got))
	}
	if got[0].Name != EventSignupStarted || got[1].Name != EventVerified {
		t.Fatal("events out of order")
	}
}

func TestMedianEvenCountIsLowerMiddle(t *testing.T) {
	d := []time.Duration{4 * time.Second, 2 * time.Second}
	if got := median(d); got != 2*time.Second {
		t.Fatalf("median = %v, want 2s (lower-middle, deterministic)", got)
	}
}
