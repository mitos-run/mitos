package main

import (
	"context"
	"log/slog"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/quota"
)

// enforcementConfig is the gateway's quota/abuse enforcement configuration,
// derived from flags and environment. It is the single place the hosted profile's
// default-on enforcement is selected and the dev/test bypass is named.
type enforcementConfig struct {
	// enabled turns the real quota.Enforcer on. It defaults on for the hosted
	// profile; an operator may disable it for a trusted single-tenant deployment,
	// in which case the gateway uses the permissive AllowAllQuota and logs the
	// mode at startup so the bypass is never silent.
	enabled bool
	// trustedProxyHops is the number of trusted reverse-proxy hops in front of the
	// gateway, governing how the client IP is resolved for the per-IP rate-limit
	// bucket. See saas.TrustedProxyHops.
	trustedProxyHops int
	// suspensions is the kill-switch store to enforce against. main wires the
	// durable, replica-shared Postgres store (wrapped in the short-TTL read cache)
	// when a database is configured, so suspensions survive restarts and bind every
	// replica. Nil falls back to an in-process MemSuspensionStore (dev only).
	suspensions quota.SuspensionStore
}

// quotaWiring is the constructed enforcement surface: the saas.QuotaEnforcer the
// gateway calls before forwarding, plus the kill-switch handles the operator and
// billing paths drive. When enforcement is disabled the enforcer is the permissive
// AllowAllQuota and the kill-switch handles are nil.
type quotaWiring struct {
	// enforcer is the seam the gateway calls. It is quota.GatewayAdapter wrapping a
	// quota.Enforcer when enabled, or saas.AllowAllQuota when disabled.
	enforcer saas.QuotaEnforcer
	// killSwitch is the suspension control the abuse path and the operator emergency
	// stop drive. It writes to the SAME store the enforcer reads, so a suspended org
	// is blocked at the gateway. Nil when enforcement is disabled.
	killSwitch *quota.KillSwitch
	// billingSuspender adapts the kill-switch to the billing.Suspender seam so a
	// breached hard spend cap or exhausted dunning suspends through the same store.
	// Nil when enforcement is disabled.
	billingSuspender *quota.BillingSuspender
	// suspensions is the kill-switch store the enforcer reads: the durable Postgres
	// store (behind the short-TTL read cache) when a database is configured, the
	// in-process MemSuspensionStore otherwise (see buildQuotaEnforcer). Nil when
	// enforcement is disabled.
	suspensions quota.SuspensionStore
	// mode is a human-legible description of the enforcement mode for the startup
	// log line, so an operator can always see whether enforcement is on.
	mode string
}

// conservativeLiveUsage is the gateway's default LiveUsageSource until the real
// cluster-backed live count (quota.LiveCounter over the controller's running-
// sandbox set) is wired into the gateway binary. It reports ZERO live footprint,
// which means the live concurrency and aggregate resource caps are NOT enforced
// anywhere today: the control plane does NOT re-check them, so an org can exceed
// its tier's concurrency and aggregate caps until the real live count is wired
// (issue #615 part 2). The per-sandbox size cap, the per-org and per-IP
// request-rate buckets, the creation-rate bucket, and the kill-switch ALL still
// apply, so this default still bounds the dominant abuse vectors (request floods,
// create churn, oversized single sandboxes, and suspended orgs); unbounded
// steady-state accumulation of small sandboxes is the gap.
//
// The controller's internal usage API (:8092) serves time-integrated usage
// records, not an instantaneous per-org running count, so an adapter over it
// would misreport live footprint; the honest fix is wiring quota.LiveCounter
// over the controller's running-sandbox set (tracked in #615).
type conservativeLiveUsage struct{}

func (conservativeLiveUsage) Live(_ context.Context, _ string) (quota.LiveUsage, error) {
	return quota.LiveUsage{}, nil
}

// freeTierResolver resolves every org to the tightest tier (TierFree). It is the
// fail-closed default until a plan-backed resolver (reading the org's billing
// plan) is wired: an org whose plan is unknown gets the SMALLEST limits and the
// deny-by-default egress posture, never the widest. Climbing the ladder is an
// explicit, plan-driven action; the absence of a plan is never read as "unlimited".
func freeTierResolver(_ context.Context, _ string) (quota.TierName, error) {
	return quota.TierFree, nil
}

// buildQuotaEnforcer constructs the gateway's quota/abuse enforcement surface from
// the configuration. When enforcement is enabled it builds the real
// quota.Enforcer over the default tier ladder, a conservative live-usage source,
// and the configured suspension store, then wraps it in the
// quota.GatewayAdapter whose IPOf seam reads the gateway-resolved client IP from
// the request context (saas.ClientIPFromContext), so the per-IP rate-limit bucket
// is charged against the trusted source address, never a spoofable header. The
// SAME suspension store backs the returned KillSwitch and BillingSuspender, so an
// abuse signal or a billing past-due suspension blocks the org at the gateway.
//
// Fail-open/closed decision for store errors: the enforcer checks the suspension
// store FIRST and returns an error if the store is unreachable; the gateway maps
// that error to a deny (quota_exceeded), so an org whose suspension state cannot be
// read is REFUSED, not silently allowed. Enforcement therefore fails CLOSED on a
// store error: a transient Postgres outage must not become an open door for a
// possibly-suspended org (the short-TTL read cache keeps recently-checked orgs
// served during a blip, bounded by the TTL).
//
// Durability: when main configures a database, cfg.suspensions is the Postgres
// PgSuspensionStore behind quota.CachedSuspensionStore, so a suspension survives
// gateway restarts and binds EVERY replica; a suspension written on one replica
// takes effect on the others within the cache TTL (a few seconds). Without a
// database (dev only) the store falls back to the in-process MemSuspensionStore,
// which is neither durable nor replica-shared; the startup log names that mode.
//
// When enforcement is disabled, the gateway uses saas.AllowAllQuota (the permissive
// stand-in) so a trusted single-tenant deployment can opt out; the mode is named in
// the returned wiring so the caller logs it at startup. The bypass is never silent.
func buildQuotaEnforcer(cfg enforcementConfig) quotaWiring {
	if !cfg.enabled {
		return quotaWiring{
			enforcer: saas.AllowAllQuota{},
			mode:     "DISABLED (permissive AllowAllQuota; every request is allowed)",
		}
	}

	sus := cfg.suspensions
	susMode := "durable Postgres kill-switch store, shared across replicas"
	if sus == nil {
		sus = quota.NewMemSuspensionStore()
		susMode = "in-process kill-switch store, NOT durable and NOT shared across replicas (DEV ONLY; set a database DSN for a durable kill-switch)"
	}
	enf := quota.NewEnforcer(quota.Deps{
		Tiers:       quota.DefaultTiers(),
		TierOf:      freeTierResolver,
		LiveUsage:   conservativeLiveUsage{},
		Suspensions: sus,
		// Now nil: the enforcer creates a real-clock rate limiter.
	})
	ks := quota.NewKillSwitch(sus, nil)
	adapter := quota.GatewayAdapter{
		Enforcer: enf,
		// IPOf reads the gateway-resolved, trusted client IP from the request
		// context. A spoofable X-Forwarded-For cannot reach this value; the gateway
		// resolved it under its trusted-proxy hop model before calling the enforcer.
		IPOf: saas.ClientIPFromContext,
	}
	return quotaWiring{
		enforcer:         adapter,
		killSwitch:       ks,
		billingSuspender: quota.NewBillingSuspender(ks),
		suspensions:      sus,
		mode:             "ENABLED (real quota.Enforcer; per-org and per-IP rate limits, per-sandbox size cap, creation-rate cap, and kill-switch; " + susMode + ")",
	}
}

// logEnforcementMode logs the constructed enforcement mode at startup so an
// operator can always see whether the gateway is enforcing quotas, without leaking
// any secret. It logs the mode string and the trusted-hop count only.
func logEnforcementMode(log *slog.Logger, cfg enforcementConfig, w quotaWiring) {
	if cfg.enabled {
		log.Info("gateway quota enforcement", "mode", w.mode, "trusted_proxy_hops", cfg.trustedProxyHops)
		return
	}
	// A disabled enforcement surface is a notable posture: warn so it stands out in
	// the logs and is never mistaken for the hosted default.
	log.Warn("gateway quota enforcement", "mode", w.mode, "trusted_proxy_hops", cfg.trustedProxyHops)
}
