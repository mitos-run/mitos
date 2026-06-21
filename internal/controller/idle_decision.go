package controller

import (
	"context"
	"time"
)

// activitySignal is the work-aware activity snapshot the lifetime reaper reads
// for one sandbox (issue #218). It carries the inbound-interaction last-activity
// AND the signals that make idle work-aware: a live set_timeout deadline, the
// count of OPEN streams (a running background job), and the paused flag.
type activitySignal struct {
	// Created is the sandbox fork time.
	Created time.Time
	// LastActivity is the most recent inbound exec or file interaction; zero
	// when the sandbox has never been touched.
	LastActivity time.Time
	// Deadline is the live TTL set via set_timeout; zero when none is set.
	Deadline time.Time
	// ActiveStreams is the number of OPEN streams (streaming exec, run_code,
	// PTY). A non-zero count means a background job is running.
	ActiveStreams int
	// Paused is true when the sandbox is held by a pause (clock stopped).
	Paused bool
}

// hasLiveWork reports whether the sandbox is doing ACTUAL work right now: a live
// background process (an open stream). This is the signal that makes idle
// work-aware: an unattended job with no inbound interaction still counts as
// busy, so it is never reaped mid-run.
func (s activitySignal) hasLiveWork() bool {
	return s.ActiveStreams > 0
}

// idleExpired reports whether the sandbox has exceeded its idle window measured
// against ACTUAL activity (issue #218). It is NOT idle when:
//   - it is paused (the clock is stopped while held), or
//   - it has a live background job (an open stream), so an unattended job is
//     never killed mid-run.
//
// Otherwise idle is measured from the later of last-activity and the sandbox
// start, exactly as the interaction-only clock did, so the default behavior for
// a truly idle sandbox is unchanged. The default idle window itself is set by
// the SandboxClaim Spec.IdleTimeout; there is no implicit default (zero means
// no idle limit).
func idleExpired(sig activitySignal, started time.Time, idle time.Duration, now time.Time) bool {
	if sig.Paused || sig.hasLiveWork() {
		return false
	}
	last := started
	if sig.LastActivity.After(last) {
		last = sig.LastActivity
	}
	return now.After(last.Add(idle))
}

// deadlineExpired reports whether a live set_timeout deadline has passed
// (issue #218). A zero deadline means none was set, so it never expires (the
// idle/maxLifetime clocks govern). A paused sandbox's deadline does not fire:
// the clock is stopped while held.
func deadlineExpired(sig activitySignal, now time.Time) bool {
	if sig.Paused || sig.Deadline.IsZero() {
		return false
	}
	return now.After(sig.Deadline)
}

// fetchActivitySignal queries the forkd on nodeName for sandboxID and returns
// the full work-aware activity snapshot. ok is false when the node is
// unreachable or the sandbox is absent, in which case the caller treats the
// idle check as not-yet-evaluable and requeues.
func fetchActivitySignal(ctx context.Context, registry *NodeRegistry, nodeName, sandboxID string) (activitySignal, bool) {
	info, ok := sandboxInfo(ctx, registry, nodeName, sandboxID)
	if !ok {
		return activitySignal{}, false
	}
	sig := activitySignal{
		ActiveStreams: int(info.ActiveStreams),
		Paused:        info.Paused,
	}
	if info.CreatedAtUnix != 0 {
		sig.Created = time.Unix(info.CreatedAtUnix, 0)
	}
	if info.LastActivityUnix != 0 {
		sig.LastActivity = time.Unix(info.LastActivityUnix, 0)
	}
	if info.DeadlineUnix != 0 {
		sig.Deadline = time.Unix(info.DeadlineUnix, 0)
	}
	return sig, true
}
