package main

import (
	"context"
	"log/slog"

	"mitos.run/mitos/internal/saas/quota"
)

// probeLiveCounter runs ONE live-count read at startup so a PERSISTENT
// misconfiguration of the gateway's cluster client (a ServiceAccount without
// the list grant on sandboxes.mitos.run, a client scheme missing mitos.run/v1)
// is loudly visible at boot instead of silently converting the whole fleet to
// deny-all-creates: the counter fails closed per request, so with a broken
// client every create is refused 429 while reads keep working, which an
// operator would otherwise only discover from customer reports. The probe org
// id is reserved and its namespace need not exist (an empty list is success).
//
// On error the probe logs at ERROR with actionable remediation (the
// LLM-legible error rule) and RETURNS: a transient apiserver blip at boot must
// not crash-loop the gateway, and the per-request fail-closed path already
// protects admission. No secret is logged; the error carries only resource and
// namespace identifiers.
func probeLiveCounter(ctx context.Context, c quota.LiveCounter, log *slog.Logger) {
	if _, err := c.Count(ctx, "startup-selfcheck"); err != nil {
		log.Error("gateway live concurrency self-check failed; sandbox creates will be DENIED (fail closed) until the live count is readable",
			"err", err.Error(),
			"remediation", "verify the gateway ServiceAccount RBAC grants list on sandboxes.mitos.run in the org namespaces and that the gateway client scheme registers mitos.run/v1; a transient apiserver error clears on its own")
		return
	}
	log.Info("gateway live concurrency self-check ok (cluster live count readable)")
}
