package controller_test

import (
	"bytes"
	"crypto/x509"
	"testing"
	"time"

	"mitos.run/mitos/internal/controller"
	"mitos.run/mitos/internal/pki"
)

// TestEnsureHuskTLSRotatesNearExpiryLeaf proves the per-namespace husk leaf is
// reissued before it expires: with the renew-before window set wide enough that
// a freshly issued leaf already counts as near expiry, a second EnsureHuskTLS
// replaces the cert rather than leaving the soon-to-expire one in place.
func TestEnsureHuskTLSRotatesNearExpiryLeaf(t *testing.T) {
	c := newCoreClient(t)
	ctrlNs := newPKINamespace(t, c)
	if _, err := controller.EnsurePKI(ctx, c, ctrlNs); err != nil {
		t.Fatal(err)
	}
	poolNs := newPKINamespace(t, c)
	if err := controller.EnsureHuskTLS(ctx, c, ctrlNs, poolNs); err != nil {
		t.Fatal(err)
	}
	first := getSecret(t, c, poolNs, controller.HuskTLSSecretName).Data["tls.crt"]

	// Force the renewal path: a fresh ~2y leaf is "near expiry" under a 100y window.
	orig := controller.HuskCertRenewBefore
	controller.HuskCertRenewBefore = 100 * 365 * 24 * time.Hour
	defer func() { controller.HuskCertRenewBefore = orig }()

	if err := controller.EnsureHuskTLS(ctx, c, ctrlNs, poolNs); err != nil {
		t.Fatal(err)
	}
	second := getSecret(t, c, poolNs, controller.HuskTLSSecretName).Data["tls.crt"]
	if bytes.Equal(first, second) {
		t.Fatal("expected the near-expiry husk leaf to be rotated, but the cert is unchanged")
	}
	// The rotated leaf must still chain to the CA with the per-namespace SAN.
	ca := getSecret(t, c, ctrlNs, controller.CASecretName)
	verifyLeaf(t, second, ca.Data["ca.crt"], pki.HuskServerName(poolNs), x509.ExtKeyUsageServerAuth)
}

// TestEnsureHuskTLSWritesPerNamespaceLeaf proves the controller issues a
// husk.<ns>.mitos server leaf signed by the control-plane CA and writes it to a
// namespace-local mitos-husk-tls Secret, so a pool namespace never needs the
// shared forkd server key to serve the husk control channel.
func TestEnsureHuskTLSWritesPerNamespaceLeaf(t *testing.T) {
	c := newCoreClient(t)
	ctrlNs := newPKINamespace(t, c)
	if _, err := controller.EnsurePKI(ctx, c, ctrlNs); err != nil {
		t.Fatalf("EnsurePKI: %v", err)
	}
	poolNs := newPKINamespace(t, c)

	if err := controller.EnsureHuskTLS(ctx, c, ctrlNs, poolNs); err != nil {
		t.Fatalf("EnsureHuskTLS: %v", err)
	}

	sec := getSecret(t, c, poolNs, controller.HuskTLSSecretName)
	for _, key := range []string{"tls.crt", "tls.key"} {
		if len(sec.Data[key]) == 0 {
			t.Fatalf("husk tls secret missing key %s", key)
		}
	}
	ca := getSecret(t, c, ctrlNs, controller.CASecretName)
	verifyLeaf(t, sec.Data["tls.crt"], ca.Data["ca.crt"], pki.HuskServerName(poolNs), x509.ExtKeyUsageServerAuth)
}

// TestHuskDialTLSConfigPinsNamespaceIdentity proves the controller builds its
// husk dial config pinning husk.<poolNamespace>.mitos (from the controller leaf
// and CA in the controller namespace), so a per-namespace husk leaf in one
// namespace cannot satisfy a dial aimed at another namespace.
func TestHuskDialTLSConfigPinsNamespaceIdentity(t *testing.T) {
	c := newCoreClient(t)
	ctrlNs := newPKINamespace(t, c)
	if _, err := controller.EnsurePKI(ctx, c, ctrlNs); err != nil {
		t.Fatalf("EnsurePKI: %v", err)
	}
	cfg, err := controller.HuskDialTLSConfig(ctx, c, ctrlNs, "tenant-a")
	if err != nil {
		t.Fatalf("HuskDialTLSConfig: %v", err)
	}
	if cfg.ServerName != pki.HuskServerName("tenant-a") {
		t.Errorf("dial ServerName = %q, want %q", cfg.ServerName, pki.HuskServerName("tenant-a"))
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("dial config has %d client certs, want 1 (the controller leaf)", len(cfg.Certificates))
	}
}

// TestEnsureHuskTLSIsIdempotent proves a second call leaves the existing leaf
// untouched (no churn of running husk pods' mounted cert).
func TestEnsureHuskTLSIsIdempotent(t *testing.T) {
	c := newCoreClient(t)
	ctrlNs := newPKINamespace(t, c)
	if _, err := controller.EnsurePKI(ctx, c, ctrlNs); err != nil {
		t.Fatal(err)
	}
	poolNs := newPKINamespace(t, c)
	if err := controller.EnsureHuskTLS(ctx, c, ctrlNs, poolNs); err != nil {
		t.Fatal(err)
	}
	first := getSecret(t, c, poolNs, controller.HuskTLSSecretName)
	if err := controller.EnsureHuskTLS(ctx, c, ctrlNs, poolNs); err != nil {
		t.Fatal(err)
	}
	second := getSecret(t, c, poolNs, controller.HuskTLSSecretName)
	if string(first.Data["tls.crt"]) != string(second.Data["tls.crt"]) {
		t.Error("husk tls leaf changed on a second EnsureHuskTLS call; it must be idempotent")
	}
}
