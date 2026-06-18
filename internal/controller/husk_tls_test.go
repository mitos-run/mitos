package controller_test

import (
	"crypto/x509"
	"testing"

	"github.com/paperclipinc/mitos/internal/controller"
	"github.com/paperclipinc/mitos/internal/pki"
)

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
