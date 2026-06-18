package controller

import (
	"context"
	"fmt"

	"github.com/paperclipinc/mitos/internal/pki"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// HuskTLSSecretName is the per-pool-namespace husk control-channel server cert
// (SAN husk.<namespace>.mitos), keys tls.crt/tls.key. Each pool namespace gets
// its own leaf with a distinct key so the shared forkd server private key is
// never replicated into a tenant namespace; the controller pins the
// per-namespace identity when dialing, so a leaked per-namespace key cannot
// impersonate a husk in another namespace.
const HuskTLSSecretName = "mitos-husk-tls"

// EnsureHuskTLS idempotently materializes the husk control-channel server leaf
// for a pool namespace: it loads the control-plane CA from the controller
// namespace (where ca.key lives and never leaves) and issues/heals a
// husk.<poolNamespace>.mitos leaf into a namespace-local mitos-husk-tls Secret
// the husk pod mounts. Reuses the same ensureCA/ensureLeaf primitives as
// EnsurePKI, so it is race-safe and self-healing. A pool that runs in the
// controller namespace itself needs nothing extra (mitos-forkd-tls is already
// there), so that case is a no-op.
func EnsureHuskTLS(ctx context.Context, c client.Client, controllerNamespace, poolNamespace string) error {
	if poolNamespace == controllerNamespace {
		return nil
	}
	ca, err := ensureCA(ctx, c, controllerNamespace)
	if err != nil {
		return fmt.Errorf("load CA to issue husk leaf for %s: %w", poolNamespace, err)
	}
	if _, err := ensureLeaf(ctx, c, poolNamespace, HuskTLSSecretName, pki.HuskServerName(poolNamespace), ca); err != nil {
		return fmt.Errorf("ensure husk tls for %s: %w", poolNamespace, err)
	}
	return nil
}
