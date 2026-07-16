package husk

import (
	"context"
	"log/slog"
	"time"
)

// huskKeepAliveInterval paces the warm-pool self-keepalive (#913). Measured on
// prod (#903): a dormant prepare-restored guest's prefaulted working set decays
// within minutes (materially within one minute), so a pod claimed long after
// Prepare pays a cold first exec despite the one-shot prefault. Re-running the
// inert cell every ~60 s holds the run_code kernel resident, the same cadence
// the pre-claimed checkout buffer's keepalive uses (internal/saas/controlplane).
const huskKeepAliveInterval = 60 * time.Second

// huskKeepAliveTimeout bounds one keepalive round so a wedged dormant guest
// costs one bounded, cancellable call per interval rather than a stuck loop. The
// cell is inert ("pass"); a warm kernel answers in single-digit milliseconds,
// and a genuinely wedged pod is caught by the liveness monitor (MonitorVMM), not
// here: this loop is best-effort warming, never a health gate.
const huskKeepAliveTimeout = 30 * time.Second

// startKeepalive launches this instance's warm-pool self-keepalive loop: every
// interval it runs the inert warm cell (firecracker.WarmKernelCode) against the
// pod's OWN running dormant guest over its local vsock, so the run_code kernel's
// working set stays resident until a claim arrives (#913, countering the #903
// idle decay of the one-shot Prepare-time prefault).
//
// It is started ONLY for a pre-restored default VM whose Prepare opted into the
// kernel prefault (prepareRestoreDefaultVM gates the call): a pod that never
// pre-restored has no running dormant guest to warm, and a pod that did not opt
// into the prefault keeps no kernel warm to preserve. The loop runs while
// DORMANT only; activate (a claim) and Close both call stopKeepalive, so it
// never contends with tenant run_code and never touches tenant state.
//
// It FAILS SOFT: a round that errors logs (paths and counts only, never
// secrets) and the next round retries; it never crashes the pod or blocks
// activate. The caller holds inst.mu; the loop goroutine captures vsockPath and
// the warm seam and touches no inst field, so it needs no lock and stopKeepalive
// (also under inst.mu) never deadlocks against it.
func (s *Stub) startKeepalive(inst *vmInstance, id vmID, vsockPath string) {
	warm := s.keepaliveWarm
	if warm == nil {
		// Production seam: reuse the Prepare-time prefault path with a self-dialed
		// connection (a long-idle vsock conn is not held across dormancy), so the
		// drain and inert-cell hygiene live in exactly one place (prefaultKernelGRPC).
		warm = func(ctx context.Context, vsock string) error {
			return prefaultKernelGRPC(ctx, nil, vsock)
		}
	}
	interval := s.keepaliveInterval
	if interval <= 0 {
		interval = huskKeepAliveInterval
	}

	// The loop outlives the Prepare context (which may be cancelled once Prepare
	// returns), so it gets its own cancellable context torn down by stopKeepalive.
	kctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	inst.keepaliveCancel = cancel
	inst.keepaliveDone = done

	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-kctx.Done():
				return
			case <-t.C:
				rctx, rcancel := context.WithTimeout(kctx, huskKeepAliveTimeout)
				err := warm(rctx, vsockPath)
				rcancel()
				if err != nil {
					// A cancellation is stopKeepalive, not a warming failure: exit
					// quietly rather than logging a spurious error at teardown/claim.
					if kctx.Err() != nil {
						return
					}
					slog.Warn("husk warm-pool keepalive round failed; the dormant guest stays and the next round retries",
						"vm", string(id), "error", err.Error())
				}
			}
		}
	}()
}

// stopKeepalive stops this instance's warm-pool keepalive loop and waits for its
// goroutine to exit, so no keepalive round is in flight once the caller proceeds
// to serve a tenant (activate) or tear the VM down (Close). Idempotent: a nil
// keepaliveCancel (no loop was running) is a no-op. The caller holds inst.mu;
// the loop goroutine takes no lock, so the wait cannot deadlock.
func (inst *vmInstance) stopKeepalive() {
	if inst.keepaliveCancel == nil {
		return
	}
	inst.keepaliveCancel()
	<-inst.keepaliveDone
	inst.keepaliveCancel = nil
	inst.keepaliveDone = nil
}
