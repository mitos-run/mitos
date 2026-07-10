package controlplane

import (
	"context"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/saas"
)

// watchFallbackInterval is the coarse re-Get cadence inside the watch loop. It
// guards against a silently dropped watch stream (missed events without a
// channel close); on the healthy path the watch delivers readiness within
// milliseconds and this ticker never fires.
const watchFallbackInterval = 2 * time.Second

// watchReady waits for the sandbox's terminal create outcome via a WATCH on
// the single object (field selector metadata.name in the org namespace), so
// the create returns the moment the controller flips the phase instead of on a
// poll-tick boundary. The overall ready timeout and every outcome envelope are
// identical to the poll path (sandboxOutcome is shared).
//
// done=false means the watch could not be established or its stream closed
// before a terminal outcome; the caller fails OPEN to the legacy poll loop,
// which re-derives the remaining deadline budget.
func (k *K8sControlPlane) watchReady(ctx context.Context, w client.WithWatch, ns, name string, startedAt time.Time, deadline time.Time) (saas.ForwardResponse, bool) {
	// A dedicated cancel bounds the watch stream's lifetime to this wait.
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	wi, err := establishSandboxWatch(watchCtx, w, ns, name)
	if err != nil {
		slog.Warn("could not establish the sandbox ready watch",
			"namespace", ns, "sandbox", name, "error", err.Error())
		return saas.ForwardResponse{}, false
	}
	defer wi.Stop()

	// Authoritative read AFTER the watch is established: a phase flip between
	// the Create and the Watch would otherwise never produce an event and the
	// wait would idle until the fallback re-Get. (The create hot path avoids
	// this read entirely by establishing its watch BEFORE the Create; this
	// path serves waits whose object predates the watch, such as fork.)
	var sb v1.Sandbox
	if err := k.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sb); err != nil {
		return readSandboxError(ctx, err), true
	}
	if resp, done := k.sandboxOutcome(ctx, &sb, startedAt); done {
		return resp, true
	}

	return k.watchWait(ctx, wi, ns, name, startedAt, deadline, phaseOrUnknown(&sb))
}

// establishSandboxWatch opens the single-object watch backing the readiness
// waits. The create hot path calls it BEFORE the Sandbox Create, so the create
// event itself arrives on the stream and no authoritative re-read is needed to
// close a missed-event window; watchReady calls it after, for objects that
// already exist.
func establishSandboxWatch(ctx context.Context, w client.WithWatch, ns, name string) (watch.Interface, error) {
	var list v1.SandboxList
	return w.Watch(ctx, &list,
		client.InNamespace(ns),
		// The api server filters to the single object server-side. The fake
		// client ignores field selectors on Watch, so the event loop re-checks
		// the name on every event regardless. Watching a name that does not
		// exist yet is valid; events begin when the object is created.
		client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector("metadata.name", name)},
	)
}

// watchWait consumes an ALREADY-ESTABLISHED single-object watch until a
// terminal create outcome, the deadline, or a closed stream (done=false: the
// caller fails open to polling for the remaining deadline budget). lastPhase
// seeds the phase a timeout envelope names before any event arrives.
func (k *K8sControlPlane) watchWait(ctx context.Context, wi watch.Interface, ns, name string, startedAt time.Time, deadline time.Time, lastPhase string) (saas.ForwardResponse, bool) {
	// The overall ready timeout: a non-positive remainder fires immediately.
	timeout := time.NewTimer(deadline.Sub(k.now()))
	defer timeout.Stop()
	fallback := time.NewTicker(watchFallbackInterval)
	defer fallback.Stop()

	for {
		select {
		case <-ctx.Done():
			return createCanceledError(), true

		case <-timeout.C:
			return k.readyTimeoutError(lastPhase), true

		case ev, open := <-wi.ResultChan():
			if !open {
				// The stream closed without a terminal outcome (apiserver
				// restart, timeout); fail open to polling for the remainder.
				return saas.ForwardResponse{}, false
			}
			evSb, ok := ev.Object.(*v1.Sandbox)
			if !ok || evSb.Namespace != ns || evSb.Name != name {
				// A bookmark or error event, or (under the fake client, which
				// does not filter watches by field selector) another sandbox
				// in the namespace.
				continue
			}
			if ev.Type == watch.Deleted {
				// A terminate raced the create.
				return sandboxRemovedError(), true
			}
			if resp, done := k.sandboxOutcome(ctx, evSb, startedAt); done {
				return resp, true
			}
			lastPhase = phaseOrUnknown(evSb)

		case <-fallback.C:
			// Coarse re-Get guarding against missed events. Error mapping is
			// identical to the poll path.
			var cur v1.Sandbox
			if err := k.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &cur); err != nil {
				return readSandboxError(ctx, err), true
			}
			if resp, done := k.sandboxOutcome(ctx, &cur, startedAt); done {
				return resp, true
			}
			lastPhase = phaseOrUnknown(&cur)
		}
	}
}
