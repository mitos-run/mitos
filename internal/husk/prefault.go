package husk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/guestgrpc"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// prefaultKernelGRPC runs the template build's inert warm cell
// (firecracker.WarmKernelCode, pinned inert so it draws no randomness the
// guest-side post-fork reseed does not know how to reseed) against a DORMANT
// pre-restored guest, so the ipykernel's working set is resident before the
// first tenant run_code instead of being demand-faulted in under the lazy UFFD
// restore (slice 3 of docs/superpowers/plans/2026-07-10-prepare-time-restore.md,
// issue #889; the idle working-set decay this counters is issue #903).
//
// It reuses the readiness connection the caller already holds when there is
// one (the same reuse the fork-correctness handshake got in #876), and dials
// vsockPath itself otherwise. The caller decides fail-open vs fail-closed; a
// missing kernel (a non-python template) is a NORMAL condition, not an error
// in the pod.
//
// No code output is ever logged or returned: the cell is a constant, and the
// stream is drained only for its terminal frames.
func prefaultKernelGRPC(ctx context.Context, conn *guestgrpc.Client, vsockPath string) error {
	client := conn
	if client == nil {
		c, err := guestgrpc.WaitReady(ctx, vsockPath, firecracker.WarmKernelTimeoutSecs*time.Second)
		if err != nil {
			return fmt.Errorf("dial guest agent for kernel prefault: %w", err)
		}
		defer func() { _ = c.Close() }() //nolint:errcheck // best-effort close of a prefault-only dial
		client = c
	}

	// Client margin over the guest-requested timeout, mirroring the template
	// build's warmKernelGRPC: the guest must get the chance to report its own
	// timeout cleanly instead of the client racing it into a context error.
	wctx, cancel := context.WithTimeout(ctx, (firecracker.WarmKernelTimeoutSecs+30)*time.Second)
	defer cancel()
	stream, err := client.Sandbox.RunCodeStream(wctx, &sandboxv1.RunCodeStreamRequest{
		Code:           firecracker.WarmKernelCode,
		Language:       "python",
		TimeoutSeconds: firecracker.WarmKernelTimeoutSecs,
	})
	if err != nil {
		return fmt.Errorf("kernel prefault RunCodeStream open: %w", err)
	}

	var kernelErr *sandboxv1.RunError
	var exitCode int32
	sawExit := false
	for {
		msg, rerr := stream.Recv()
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return fmt.Errorf("kernel prefault RunCodeStream recv: %w", rerr)
		}
		switch v := msg.Msg.(type) {
		case *sandboxv1.RunCodeResponse_Error:
			kernelErr = v.Error
		case *sandboxv1.RunCodeResponse_ExitCode:
			exitCode = v.ExitCode
			sawExit = true
		}
	}
	if kernelErr != nil {
		return fmt.Errorf("kernel prefault cell failed: %s: %s", kernelErr.GetName(), kernelErr.GetValue())
	}
	if !sawExit {
		return errors.New("kernel prefault stream ended without an exit_code frame")
	}
	if exitCode != 0 {
		return fmt.Errorf("kernel prefault cell exited %d", exitCode)
	}
	return nil
}
