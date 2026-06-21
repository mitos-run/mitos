package quota

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Typed denial reasons. The enforcer returns one of these so the gateway adapter
// maps each to the right public apierr code (quota_exceeded vs rate_limited vs
// forbidden), and tests discriminate the exact cap that fired. None ever carries
// a secret value.
var (
	// ErrSuspended: the org is suspended (the kill switch fired). Maps to forbidden:
	// the credential is valid but the org may not act at all.
	ErrSuspended = errors.New("quota: organization is suspended")
	// ErrConcurrencyExceeded: the org is at its concurrent-sandbox cap.
	ErrConcurrencyExceeded = errors.New("quota: concurrent sandbox limit reached")
	// ErrAggregateExceeded: the create would push the org's aggregate footprint over
	// the tier cap.
	ErrAggregateExceeded = errors.New("quota: aggregate resource limit reached")
	// ErrSandboxTooLarge: the requested single-sandbox size exceeds the tier max.
	ErrSandboxTooLarge = errors.New("quota: requested sandbox exceeds the per-sandbox size limit")
	// ErrRateLimited: a request-rate or creation-rate bucket is empty.
	ErrRateLimited = errors.New("quota: request rate limit exceeded")
	// ErrUnknownTier: the org resolved to a tier not in the table (fail closed).
	ErrUnknownTier = errors.New("quota: organization tier is not configured")
)

// LiveUsage is the org's CURRENT live footprint, read from the live-usage /
// live-count seam (issue #211's records, or a direct live-count of the org's
// running sandboxes). Concurrency and aggregate caps are enforced against THIS,
// not against a stale tally, so the cap reflects what the org is actually running
// right now.
type LiveUsage struct {
	ConcurrentSandboxes int
	VCPUs               int32
	MemBytes            int64
	StorageBytes        int64
}

// LiveUsageSource is the seam the enforcer reads live footprint from. The
// production implementation aggregates the controller's running-sandbox set (or
// the #211 usage store's most recent window) per org; the test uses a static
// fake. It is org-scoped: it only ever returns the named org's usage.
type LiveUsageSource interface {
	Live(ctx context.Context, orgID string) (LiveUsage, error)
}

// LiveUsageFunc adapts a function to a LiveUsageSource so a caller can pass a
// closure over the controller's running-sandbox set or the #211 usage store.
type LiveUsageFunc func(ctx context.Context, orgID string) (LiveUsage, error)

// Live calls the wrapped function.
func (f LiveUsageFunc) Live(ctx context.Context, orgID string) (LiveUsage, error) {
	return f(ctx, orgID)
}

// SandboxSpec is the resource shape of a sandbox a create request asks for. It is
// the input the per-sandbox-size and aggregate caps check against.
type SandboxSpec struct {
	VCPUs        int32
	MemBytes     int64
	StorageBytes int64
}

// Request is the full input to an enforcement decision: the op, the caller IP
// (for the per-IP rate limit), and, for a create, the requested sandbox shape.
type Request struct {
	Op         string
	IP         string
	NewSandbox SandboxSpec
}

// isCreate reports whether the op creates a sandbox (the lifecycle, size, and
// aggregate caps apply only to creates).
func (r Request) isCreate() bool { return r.Op == "sandbox.create" }

// isLifecycle reports whether the op is an expensive lifecycle operation that
// charges the lifecycle rate bucket (create, terminate, fork). Everything else is
// in-sandbox traffic.
func (r Request) isLifecycle() bool {
	switch r.Op {
	case "sandbox.create", "sandbox.terminate", "sandbox.fork":
		return true
	default:
		return false
	}
}

// TierResolver maps an org to its current tier (plan/top-up level). The
// production implementation reads the org's billing/plan record; the test uses a
// fixed function. It fails closed: an unresolvable org is an error, not a default
// to the widest tier.
type TierResolver func(ctx context.Context, orgID string) (TierName, error)

// Deps are the enforcer's injected collaborators. Every one is a seam so the
// enforcer is unit-tested without a cluster, a billing backend, or a clock.
type Deps struct {
	Tiers       map[TierName]Tier
	TierOf      TierResolver
	LiveUsage   LiveUsageSource
	Suspensions SuspensionStore
	RateLimiter *RateLimiter
	Now         func() time.Time
}

// Enforcer is the real QuotaEnforcer: it resolves the org's tier, fails closed if
// the org is suspended, enforces the per-sandbox size cap and the live
// concurrency and aggregate caps, and charges the per-org and per-IP rate
// buckets. It is the verifiable core of the abuse-control envelope and plugs into
// the #210 gateway seam via GatewayAdapter.
type Enforcer struct {
	tiers map[TierName]Tier
	tier  TierResolver
	live  LiveUsageSource
	sus   SuspensionStore
	rl    *RateLimiter
}

// NewEnforcer builds an enforcer from its deps. A nil rate limiter is created
// with the supplied clock.
func NewEnforcer(d Deps) *Enforcer {
	rl := d.RateLimiter
	if rl == nil {
		rl = NewRateLimiter(d.Now)
	}
	return &Enforcer{
		tiers: d.Tiers,
		tier:  d.TierOf,
		live:  d.LiveUsage,
		sus:   d.Suspensions,
		rl:    rl,
	}
}

// Check is the enforcement decision. Order matters and is fail-closed:
//  1. Suspension: a suspended org is denied before any quota math (the kill
//     switch).
//  2. Tier resolution: an unresolvable org is denied (no default to a wide tier).
//  3. Per-sandbox size: a create larger than the tier max is denied.
//  4. Live concurrency and aggregate caps: read against the org's CURRENT live
//     usage (issue #211 seam), so the cap reflects what is actually running.
//  5. Rate limits: the per-org and per-IP request-rate and creation-rate buckets.
//
// A nil error means allow. Every non-nil error is one of the typed reasons above,
// which the gateway adapter maps to the public envelope.
func (e *Enforcer) Check(ctx context.Context, orgID string, req Request) error {
	// 1. Kill switch: a suspended org fails closed.
	if e.sus != nil {
		if _, suspended, err := e.sus.IsSuspended(ctx, orgID); err != nil {
			return fmt.Errorf("check suspension: %w", err)
		} else if suspended {
			return ErrSuspended
		}
	}

	// 2. Resolve the org's tier (fail closed).
	tierName, err := e.tier(ctx, orgID)
	if err != nil {
		return fmt.Errorf("resolve tier: %w", err)
	}
	tier, ok := e.tiers[tierName]
	if !ok {
		return ErrUnknownTier
	}

	// 3 and 4 apply only to creates (the only op that adds footprint).
	if req.isCreate() {
		if err := e.checkSize(tier, req.NewSandbox); err != nil {
			return err
		}
		if err := e.checkLive(ctx, tier, orgID, req.NewSandbox); err != nil {
			return err
		}
	}

	// 5. Rate limits: per-org and per-IP, lifecycle vs in-sandbox buckets, plus the
	// creation-rate bucket for creates.
	if err := e.checkRates(tier, orgID, req); err != nil {
		return err
	}
	return nil
}

// checkRates charges the request-rate buckets (per-org and per-IP) and, for a
// create, the creation-rate bucket. Any empty bucket is a rate-limit denial. It
// charges the buckets AFTER the quota checks so a denied-by-quota request does
// not consume rate budget; rate is the last gate.
func (e *Enforcer) checkRates(tier Tier, orgID string, req Request) error {
	kind := BucketInSandbox
	if req.isLifecycle() {
		kind = BucketLifecycle
	}
	// Per-org request-rate bucket.
	if !e.rl.Allow("org:"+orgID, kind, tier.APIRequestsPerMinute, tier.APIRequestsPerMinute) {
		return fmt.Errorf("%w: per-organization request rate", ErrRateLimited)
	}
	// Per-IP request-rate bucket (same tier rate; an org behind one IP is also
	// bounded per IP so a single source cannot dominate).
	if req.IP != "" {
		if !e.rl.Allow("ip:"+req.IP, kind, tier.APIRequestsPerMinute, tier.APIRequestsPerMinute) {
			return fmt.Errorf("%w: per-IP request rate", ErrRateLimited)
		}
	}
	// Creation-rate bucket for creates: a separate, tighter ladder that throttles
	// churn-create abuse independent of the API request rate.
	if req.isCreate() {
		if !e.rl.Allow("org-create:"+orgID, BucketLifecycle, tier.CreationRatePerMinute, tier.CreationRatePerMinute) {
			return fmt.Errorf("%w: sandbox creation rate", ErrRateLimited)
		}
	}
	return nil
}

// checkSize enforces the per-sandbox size cap. A zero tier cap means uncapped.
func (e *Enforcer) checkSize(tier Tier, s SandboxSpec) error {
	if tier.MaxSandboxVCPUs > 0 && s.VCPUs > tier.MaxSandboxVCPUs {
		return fmt.Errorf("%w: requested %d vCPUs exceeds the per-sandbox limit %d", ErrSandboxTooLarge, s.VCPUs, tier.MaxSandboxVCPUs)
	}
	if tier.MaxSandboxMemBytes > 0 && s.MemBytes > tier.MaxSandboxMemBytes {
		return fmt.Errorf("%w: requested memory exceeds the per-sandbox limit", ErrSandboxTooLarge)
	}
	if tier.MaxSandboxStorageBytes > 0 && s.StorageBytes > tier.MaxSandboxStorageBytes {
		return fmt.Errorf("%w: requested storage exceeds the per-sandbox limit", ErrSandboxTooLarge)
	}
	return nil
}

// checkLive enforces the concurrency and aggregate caps against the org's live
// usage plus the sandbox about to be created.
func (e *Enforcer) checkLive(ctx context.Context, tier Tier, orgID string, s SandboxSpec) error {
	live, err := e.live.Live(ctx, orgID)
	if err != nil {
		return fmt.Errorf("read live usage: %w", err)
	}
	if tier.MaxConcurrentSandboxes > 0 && live.ConcurrentSandboxes+1 > tier.MaxConcurrentSandboxes {
		return fmt.Errorf("%w: %d running, limit %d", ErrConcurrencyExceeded, live.ConcurrentSandboxes, tier.MaxConcurrentSandboxes)
	}
	if tier.MaxAggregateVCPUs > 0 && live.VCPUs+s.VCPUs > tier.MaxAggregateVCPUs {
		return fmt.Errorf("%w: aggregate vCPUs would exceed %d", ErrAggregateExceeded, tier.MaxAggregateVCPUs)
	}
	if tier.MaxAggregateMemBytes > 0 && live.MemBytes+s.MemBytes > tier.MaxAggregateMemBytes {
		return fmt.Errorf("%w: aggregate memory would exceed the tier limit", ErrAggregateExceeded)
	}
	if tier.MaxAggregateStorageBytes > 0 && live.StorageBytes+s.StorageBytes > tier.MaxAggregateStorageBytes {
		return fmt.Errorf("%w: aggregate storage would exceed the tier limit", ErrAggregateExceeded)
	}
	return nil
}
