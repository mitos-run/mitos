package agentcli

import (
	"context"
	"errors"
	"time"
)

// The mitos CLI exit-code contract. These constants are the single source of
// truth for the process exit codes and are documented in docs/cli.md. They are
// stable: an agent or shell pipeline can branch on them without parsing stderr.
//
//	ExitOK       the command succeeded. For `run` this is the executed
//	             command's own exit code (which is 0 on success).
//	ExitError    a general, remediable runtime error (backend unreachable, a
//	             failed operation). The stderr diagnostic carries the cause.
//	ExitUsage    a usage error: an unknown subcommand, a missing argument, a
//	             bad flag, or an unknown output format.
//	ExitNotFound the targeted sandbox or workspace does not exist.
//	ExitTimeout  a --wait/--timeout deadline elapsed before the operation
//	             completed. The value matches the coreutils `timeout` tool so it
//	             is familiar in shell pipelines.
const (
	ExitOK       = 0
	ExitError    = 1
	ExitUsage    = 2
	ExitNotFound = 3
	ExitTimeout  = 124
)

// ErrNotFound is the sentinel a Backend wraps (with %w) when the targeted
// sandbox or workspace does not exist, so the CLI can map it to ExitNotFound
// without string matching. It never carries a secret value.
var ErrNotFound = errors.New("not found")

// exitCodeFor classifies a backend or runtime error into the documented exit
// code. A nil error is ExitOK. A deadline (from a --timeout bound or a canceled
// context deadline) is ExitTimeout. A wrapped ErrNotFound is ExitNotFound.
// Everything else is the general ExitError.
func exitCodeFor(err error) int {
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, context.DeadlineExceeded):
		return ExitTimeout
	case errors.Is(err, ErrNotFound):
		return ExitNotFound
	default:
		return ExitError
	}
}

// lifecycleCtxKey is the private context key type for the CLI lifecycle seam.
type lifecycleCtxKey int

const noWaitKey lifecycleCtxKey = iota

// withNoWait marks ctx so a Backend that would otherwise poll for readiness
// returns as soon as the object is created. It is set by the --no-wait /
// --wait=false lifecycle flags.
func withNoWait(ctx context.Context) context.Context {
	return context.WithValue(ctx, noWaitKey, true)
}

// noWaitRequested reports whether the caller asked not to wait for readiness.
func noWaitRequested(ctx context.Context) bool {
	v, _ := ctx.Value(noWaitKey).(bool)
	return v
}

// lifecycleContext derives the operation context for a lifecycle verb from its
// --wait/--no-wait/--timeout flags. When wait is false or noWait is true the
// returned context carries the no-wait signal. When timeoutSec is positive the
// context carries a deadline; on expiry the backend call returns a
// deadline-exceeded error that classifies to ExitTimeout. The returned cancel
// func must always be called.
func lifecycleContext(ctx context.Context, wait, noWait bool, timeoutSec int) (context.Context, context.CancelFunc) {
	if noWait || !wait {
		ctx = withNoWait(ctx)
	}
	if timeoutSec > 0 {
		return context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	}
	return ctx, func() {}
}
