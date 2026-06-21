// Package onboarding is the self-serve onboarding funnel for the hosted offering
// (issue #215): it ties together sign-up, email verification, auto-creation of a
// Personal organization (Daytona-style), the free-tier signup credit grant (the
// #212 ledger), and issuance of the first API key (#210), so a brand-new user
// reaches a first successful run_code in minutes with no card on the free tier
// and exactly one SDK package.
//
// The package is built as a tested core with clean seams: the actual email SEND
// is an EmailSender interface with a fake for tests (the real SMTP/provider is a
// follow-up), and the funnel instrumentation is an EventRecorder interface with
// an in-memory implementation. Funnel statistics (conversion rate and
// time-to-first-sandbox per step) are aggregated from the recorded events so the
// "best self-serve UX" claim is verified, not asserted.
//
// Until the production gates pass (#163 chaos and residual GC, #194 external
// security review, and multitenancy, tracked by the #208 epic), the funnel runs
// in WAITLIST mode: a signup records a waitlist entry instead of provisioning. A
// deployment flips to OPEN self-serve only once those gates are green.
//
// Security: an email address is PII and a verify token is a secret. Tokens are
// stored only as a hash, never logged or placed in an error; raw emails are not
// logged. The store, the email sender, the clock, and the id/token generators
// are all injectable so the whole flow is deterministic and unit-tested without a
// database, an SMTP server, or a live clock.
package onboarding

import (
	"context"
	"sort"
	"sync"
	"time"
)

// EventName names a single step in the onboarding funnel. The set is the ordered
// funnel: a signup that reaches FirstExec has traversed every step. The names are
// stable analytics identifiers; they carry no PII.
type EventName string

const (
	// EventSignupStarted: an account began signup (email submitted, unverified).
	EventSignupStarted EventName = "signup_started"
	// EventWaitlisted: signup landed on the waitlist (waitlist mode), not provisioned.
	EventWaitlisted EventName = "waitlisted"
	// EventVerified: the email verify token was accepted and the org provisioned.
	EventVerified EventName = "verified"
	// EventKeyIssued: the first API key was issued for the new org.
	EventKeyIssued EventName = "key_issued"
	// EventFirstSandboxCreated: the org created its first sandbox.
	EventFirstSandboxCreated EventName = "first_sandbox_created"
	// EventFirstExec: the org ran code in a sandbox for the first time (the
	// time-to-first-sandbox funnel terminates here).
	EventFirstExec EventName = "first_exec"
)

// FunnelOrder is the canonical step order used by the stats aggregation to
// compute step-to-step conversion. EventWaitlisted is a side branch, not part of
// the linear provisioning funnel, so it is excluded from the conversion order.
var FunnelOrder = []EventName{
	EventSignupStarted,
	EventVerified,
	EventKeyIssued,
	EventFirstSandboxCreated,
	EventFirstExec,
}

// Event is one recorded funnel step for one subject. Subject is the funnel key:
// the account id once known, or a pre-account signup id at the very first step,
// so a single signup's progress can be followed end to end. At is the event
// time. Event carries NO email and NO token; it is safe to ship to an analytics
// sink.
type Event struct {
	Subject string
	Name    EventName
	At      time.Time
}

// EventRecorder is the funnel instrumentation seam. The in-memory implementation
// is the tested default; a real analytics sink (the live time-to-first-sandbox
// dashboard) is a documented follow-up behind the same interface. Implementations
// must be safe for concurrent use and must never persist PII or secrets.
type EventRecorder interface {
	// Record appends a funnel event. It never blocks the funnel on failure in a
	// real sink; the in-memory impl always succeeds.
	Record(ctx context.Context, e Event)
	// Events returns every recorded event in append order, for aggregation and tests.
	Events(ctx context.Context) []Event
}

// MemEventRecorder is the in-memory EventRecorder used as the tested default.
// Safe for concurrent use.
type MemEventRecorder struct {
	mu     sync.Mutex
	events []Event
}

// NewMemEventRecorder returns an empty in-memory recorder.
func NewMemEventRecorder() *MemEventRecorder {
	return &MemEventRecorder{}
}

// Record appends an event.
func (r *MemEventRecorder) Record(_ context.Context, e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// Events returns a copy of the recorded events in append order.
func (r *MemEventRecorder) Events(_ context.Context) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.events))
	copy(out, r.events)
	return out
}

// nopRecorder is the default recorder when a caller supplies none: it discards
// events so the funnel still runs without instrumentation configured.
type nopRecorder struct{}

func (nopRecorder) Record(_ context.Context, _ Event) {}
func (nopRecorder) Events(_ context.Context) []Event  { return nil }

// StepStats is the aggregated funnel stats for one step transition.
type StepStats struct {
	// From and To are the funnel steps this transition spans.
	From EventName
	To   EventName
	// Reached is how many distinct subjects reached the From step.
	Reached int
	// Converted is how many of those subjects went on to reach the To step.
	Converted int
	// MedianTime is the median elapsed time from the From event to the To event
	// across subjects that made the transition. Zero when none did.
	MedianTime time.Duration
}

// ConversionRate is Converted/Reached as a fraction in [0,1]. Zero when no
// subject reached the From step.
func (s StepStats) ConversionRate() float64 {
	if s.Reached == 0 {
		return 0
	}
	return float64(s.Converted) / float64(s.Reached)
}

// FunnelStats is the whole-funnel aggregation: per-step conversion plus the
// end-to-end time-to-first-sandbox (signup_started -> first_exec).
type FunnelStats struct {
	// Steps is the per-transition stats in FunnelOrder.
	Steps []StepStats
	// SignupStarted is the count of distinct subjects that entered the funnel.
	SignupStarted int
	// FirstExec is the count of distinct subjects that reached first_exec.
	FirstExec int
	// MedianTimeToFirstExec is the median signup_started -> first_exec elapsed
	// time across subjects that completed the funnel. This is the headline
	// time-to-first-sandbox metric. Zero when none completed.
	MedianTimeToFirstExec time.Duration
}

// OverallConversionRate is FirstExec/SignupStarted in [0,1]. Zero when nobody
// started.
func (f FunnelStats) OverallConversionRate() float64 {
	if f.SignupStarted == 0 {
		return 0
	}
	return float64(f.FirstExec) / float64(f.SignupStarted)
}

// AggregateFunnel computes funnel statistics from a flat event list. It groups
// events by subject, takes the FIRST timestamp seen for each (subject, step)
// pair (so a replayed event does not skew timing), and computes per-step
// conversion and median transition times along FunnelOrder, plus the end-to-end
// time-to-first-sandbox. It is pure and deterministic: same events in, same
// stats out.
func AggregateFunnel(events []Event) FunnelStats {
	// first[subject][step] = earliest time that subject hit that step.
	first := map[string]map[EventName]time.Time{}
	for _, e := range events {
		bySubj := first[e.Subject]
		if bySubj == nil {
			bySubj = map[EventName]time.Time{}
			first[e.Subject] = bySubj
		}
		if prev, ok := bySubj[e.Name]; !ok || e.At.Before(prev) {
			bySubj[e.Name] = e.At
		}
	}

	stats := FunnelStats{}
	for i := 0; i+1 < len(FunnelOrder); i++ {
		from, to := FunnelOrder[i], FunnelOrder[i+1]
		s := StepStats{From: from, To: to}
		var durs []time.Duration
		for _, steps := range first {
			fAt, hasFrom := steps[from]
			if !hasFrom {
				continue
			}
			s.Reached++
			tAt, hasTo := steps[to]
			if !hasTo || tAt.Before(fAt) {
				continue
			}
			s.Converted++
			durs = append(durs, tAt.Sub(fAt))
		}
		s.MedianTime = median(durs)
		stats.Steps = append(stats.Steps, s)
	}

	var e2e []time.Duration
	for _, steps := range first {
		start, hasStart := steps[EventSignupStarted]
		if hasStart {
			stats.SignupStarted++
		}
		end, hasEnd := steps[EventFirstExec]
		if hasEnd {
			stats.FirstExec++
		}
		if hasStart && hasEnd && !end.Before(start) {
			e2e = append(e2e, end.Sub(start))
		}
	}
	stats.MedianTimeToFirstExec = median(e2e)
	return stats
}

// median returns the median of durations, or zero for an empty slice. For an
// even count it returns the lower-middle element (a deterministic, integer
// choice; no averaging that could surprise the caller).
func median(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[(len(d)-1)/2]
}
