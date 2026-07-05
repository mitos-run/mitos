package quota

import (
	"context"
	"errors"

	"mitos.run/mitos/internal/apierr"
)

// GatewayAdapter plugs the Enforcer into the #210 gateway's QuotaEnforcer seam
// (saas.QuotaEnforcer). The seam is intentionally narrow: Check(ctx, orgID, op)
// returns an error to deny. The adapter widens the gateway call into a quota
// Request: it derives the caller IP from the request context (via IPOf) for the
// per-IP rate limit and the requested sandbox shape from SizeOf (when wired).
//
// HONEST LIMIT (issue #615): this front-door check is the ONLY enforcement
// point for the tier caps; the control plane does NOT re-check them. With
// SizeOf unwired (the shipped gateway today) a create is checked with a ZERO
// spec, so only the live concurrency cap can trip; the per-sandbox size cap
// and the aggregate resource caps are NOT enforced until SizeOf and the
// pool-resolved live footprint are wired (deferred follow-ups).
//
// The adapter maps each typed enforcer denial to the right public apierr code:
//   - suspension and unknown-tier -> forbidden (the org may not act at all).
//   - rate-limit denials -> rate_limited.
//   - every quota cap (concurrency, aggregate, size) -> quota_exceeded.
//
// It NEVER logs a key or secret; the gateway already logs the key id and op.
type GatewayAdapter struct {
	Enforcer *Enforcer
	// IPOf extracts the caller IP from the request context for the per-IP rate
	// limit. A nil IPOf yields an empty IP, which disables the per-IP bucket (the
	// per-org bucket still applies).
	IPOf func(ctx context.Context) string
	// SizeOf optionally derives the requested sandbox shape for a create from the
	// context (the gateway can stash the parsed body). When nil, the adapter uses
	// the tier-max conservative shape via the enforcer's create path with a zero
	// spec, which only trips the live concurrency cap, not the size cap; set SizeOf
	// to enforce the size and aggregate caps at the front door.
	SizeOf func(ctx context.Context) (SandboxSpec, bool)
}

// Check satisfies saas.QuotaEnforcer. It builds a Request from the op and the
// context, runs the enforcer, and maps the typed denial to a public code by
// returning a sentinel error the gateway encodes. The gateway maps any non-nil
// error to quota_exceeded today; this adapter additionally carries the precise
// code so a future gateway that inspects it (or a direct caller) gets the right
// envelope.
func (a GatewayAdapter) Check(ctx context.Context, orgID, op string) error {
	req := Request{Op: op}
	if a.IPOf != nil {
		req.IP = a.IPOf(ctx)
	}
	// A live fork (sandbox.fork) admits a new sandbox exactly like a create
	// (Request.isCreate classes both), so the requested-shape seam applies to
	// both: a wired SizeOf must cap forks too, or an org could exceed the size
	// caps by forking instead of creating.
	if (op == "sandbox.create" || op == "sandbox.fork") && a.SizeOf != nil {
		if spec, ok := a.SizeOf(ctx); ok {
			req.NewSandbox = spec
		}
	}
	err := a.Enforcer.Check(ctx, orgID, req)
	if err == nil {
		return nil
	}
	// Attach the precise public envelope so a caller that inspects the error gets
	// the right code; the gateway's default mapping is quota_exceeded, which is
	// correct for the cap denials.
	return &Denial{Err: err, Envelope: envelopeFor(err)}
}

// Denial wraps a typed enforcer error with the public apierr envelope the gateway
// should emit. It implements error and unwraps to the typed cause so callers can
// still errors.Is against ErrRateLimited, ErrSuspended, and the cap sentinels.
type Denial struct {
	Err      error
	Envelope apierr.Error
}

func (d *Denial) Error() string { return d.Err.Error() }
func (d *Denial) Unwrap() error { return d.Err }

// APIError satisfies the gateway's apiErrorProvider seam so the gateway emits the
// precise public envelope (quota_exceeded, rate_limited, or forbidden) this
// denial carries.
func (d *Denial) APIError() apierr.Error { return d.Envelope }

// envelopeFor maps a typed enforcer error to its public apierr envelope.
func envelopeFor(err error) apierr.Error {
	switch {
	case errors.Is(err, ErrSuspended), errors.Is(err, ErrUnknownTier):
		return apierr.Get(apierr.CodeForbidden).
			WithCause("the organization is suspended or has no configured plan")
	case errors.Is(err, ErrRateLimited):
		return apierr.Get(apierr.CodeRateLimited).
			WithCause("the organization or source IP exceeded its request rate")
	case errors.Is(err, ErrConcurrencyExceeded),
		errors.Is(err, ErrAggregateExceeded),
		errors.Is(err, ErrSandboxTooLarge):
		return apierr.Get(apierr.CodeQuotaExceeded).
			WithCause("the organization exceeded a hosted-plan quota")
	default:
		return apierr.Get(apierr.CodeQuotaExceeded).
			WithCause("the organization quota check denied the request")
	}
}
