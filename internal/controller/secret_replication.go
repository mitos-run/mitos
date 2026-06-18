package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ReplicateHuskSecrets projects the CA certificate husk pods mount from the
// controller namespace (src) into a pool namespace (dst). Husk pods run in the
// pool namespace (e.g. default), not the controller namespace, and mount the CA
// to verify the controller's client certificate on the mTLS control channel.
//
// It projects ONLY ca.crt: the CA private key (ca.key) must never leave the
// controller namespace. The husk control-channel SERVING cert is NOT replicated
// here; EnsureHuskTLS issues a per-namespace leaf (mitos-husk-tls, SAN
// husk.<ns>.mitos) into the pool namespace instead, so the shared forkd server
// private key is never copied into a tenant namespace (a key leaked in one
// namespace cannot impersonate a husk in another).
//
// Replication is idempotent and heals drift: a destination copy whose data
// differs from the source is updated in place (so a CA rotation propagates).
// Copying into the source namespace itself is a noop (the originals are there).
func ReplicateHuskSecrets(ctx context.Context, c client.Client, src, dst string) error {
	if src == dst {
		return nil
	}
	// Only ca.crt is projected into a pool namespace. The husk control-channel
	// serving cert is a PER-NAMESPACE leaf (mitos-husk-tls, SAN husk.<ns>.mitos)
	// issued by EnsureHuskTLS, so the shared forkd server private key is never
	// replicated into a tenant namespace: a key leaked in one namespace cannot
	// impersonate a husk in another.
	return replicateControlPlaneSecret(ctx, c, src, dst, CASecretName, []string{"ca.crt"})
}

// replicateControlPlaneSecret copies exactly the named keys of secret `name`
// from src to dst, creating the destination when absent and updating it when
// its projected data drifts. Keys not in `keys` are never copied (so the CA
// private key cannot leak). A missing source secret is an error: the caller
// runs this only after EnsurePKI has materialized the originals.
func replicateControlPlaneSecret(ctx context.Context, c client.Client, src, dst, name string, keys []string) error {
	var source corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: src, Name: name}, &source); err != nil {
		return fmt.Errorf("read source secret %s/%s for replication: %w", src, name, err)
	}

	projected := make(map[string][]byte, len(keys))
	for _, k := range keys {
		v, ok := source.Data[k]
		if !ok {
			return fmt.Errorf("source secret %s/%s lacks key %s; cannot replicate", src, name, k)
		}
		projected[k] = append([]byte(nil), v...)
	}

	var existing corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Namespace: dst, Name: name}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		copySecret := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: dst, Name: name},
			Type:       source.Type,
			Data:       projected,
		}
		if err := c.Create(ctx, &copySecret); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Lost the create race to a parallel reconcile; the winner wrote
				// the same projected data from the same source, so treat as done.
				return nil
			}
			return fmt.Errorf("create replicated secret %s/%s: %w", dst, name, err)
		}
		return nil

	case err != nil:
		return fmt.Errorf("read destination secret %s/%s: %w", dst, name, err)

	default:
		if secretDataEqual(existing.Data, projected) {
			return nil
		}
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		// Overwrite exactly the projected keys; leave any unrelated keys the
		// destination may carry untouched.
		for k, v := range projected {
			existing.Data[k] = v
		}
		if err := c.Update(ctx, &existing); err != nil {
			return fmt.Errorf("heal replicated secret %s/%s: %w", dst, name, err)
		}
		return nil
	}
}

// secretDataEqual reports whether dst already contains every key/value in want.
// It does not require dst to be a strict equal (dst may carry extra keys); it
// only checks the projected keys match, which is the replication contract.
func secretDataEqual(dst, want map[string][]byte) bool {
	for k, v := range want {
		got, ok := dst[k]
		if !ok || string(got) != string(v) {
			return false
		}
	}
	return true
}
