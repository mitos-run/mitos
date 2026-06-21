package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"mitos.run/mitos/internal/pki"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// HuskCertRenewBefore is how long before a per-namespace husk leaf's NotAfter
// EnsureHuskTLS reissues it. pki leaves are valid for 2 years, so a 30-day
// window rotates well ahead of expiry across reconciles. A package var so tests
// can widen it to force the renewal path deterministically.
var HuskCertRenewBefore = 30 * 24 * time.Hour

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
	leaf, err := ensureLeaf(ctx, c, poolNamespace, HuskTLSSecretName, pki.HuskServerName(poolNamespace), ca)
	if err != nil {
		return fmt.Errorf("ensure husk tls for %s: %w", poolNamespace, err)
	}
	// Rotate ahead of expiry: ensureLeaf only heals an unusable leaf (wrong keys,
	// tampered, wrong CA), not a valid-but-aging one. Without this a long-lived
	// pool would keep serving a leaf until it expired and the control channel
	// broke. Reissue when the current leaf is within HuskCertRenewBefore of
	// NotAfter; a parse failure is treated as needs-renew (fail toward a fresh,
	// verifiable leaf).
	if huskLeafNeedsRenew(leaf.CertPEM, time.Now()) {
		fresh, err := ca.Issue(pki.HuskServerName(poolNamespace))
		if err != nil {
			return fmt.Errorf("reissue husk leaf for %s: %w", poolNamespace, err)
		}
		var sec corev1.Secret
		if err := c.Get(ctx, types.NamespacedName{Namespace: poolNamespace, Name: HuskTLSSecretName}, &sec); err != nil {
			return fmt.Errorf("read husk tls secret %s for rotation: %w", poolNamespace, err)
		}
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		sec.Data["tls.crt"] = fresh.CertPEM
		sec.Data["tls.key"] = fresh.KeyPEM
		if err := c.Update(ctx, &sec); err != nil {
			return fmt.Errorf("rotate husk tls secret %s: %w", poolNamespace, err)
		}
	}
	return nil
}

// huskLeafNeedsRenew reports whether the PEM leaf is unparseable or within
// HuskCertRenewBefore of its NotAfter as of now.
func huskLeafNeedsRenew(certPEM []byte, now time.Time) bool {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return true
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return true
	}
	return now.Add(HuskCertRenewBefore).After(cert.NotAfter)
}

// HuskDialTLSConfig builds the controller client mTLS config for dialing a husk
// pod in poolNamespace, pinning the per-namespace husk server identity
// (husk.<poolNamespace>.mitos). It loads the controller client leaf and the CA
// from the controller namespace (re-read each dial so a cert rotation is picked
// up). Because the pinned name encodes the namespace, a husk serving leaf from
// one namespace cannot satisfy a dial aimed at another, so a per-namespace key
// leak is contained to that namespace.
func HuskDialTLSConfig(ctx context.Context, c client.Client, controllerNamespace, poolNamespace string) (*tls.Config, error) {
	ca, err := ensureCA(ctx, c, controllerNamespace)
	if err != nil {
		return nil, fmt.Errorf("load CA for husk dial to %s: %w", poolNamespace, err)
	}
	leaf, err := ensureLeaf(ctx, c, controllerNamespace, ControllerTLSSecretName, pki.ControllerName, ca)
	if err != nil {
		return nil, fmt.Errorf("load controller leaf for husk dial to %s: %w", poolNamespace, err)
	}
	return pki.ClientTLSConfigFor(leaf.CertPEM, leaf.KeyPEM, ca.CertPEM(), pki.HuskServerName(poolNamespace))
}
