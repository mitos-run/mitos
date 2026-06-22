// Package kubesecrets is the kube SecretStore provider: it materializes org
// secrets as namespaced Kubernetes Secrets, the self-host default backend behind
// the console.SecretStore seam (spec §8). Each org's secrets live in that org's
// namespace, so cross-org isolation is enforced by the namespace boundary plus
// an org label; the controller (which already has scoped Secret access in pool
// namespaces via the mitos-pool-secrets RBAC) resolves the value into a sandbox
// via the existing SandboxTemplate env valueFrom path.
//
// Values are stored only in the Secret (for the controller to resolve); the
// console.SecretView this provider returns never carries the value, so the BFF
// stays write-only.
package kubesecrets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"mitos.run/mitos/internal/saas/console"
)

const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	managedByValue = "mitos-console"
	labelOrg       = "mitos.run/org"
	annName        = "mitos.run/secret-name"
	annVersion     = "mitos.run/secret-version"
	annFingerprint = "mitos.run/secret-fingerprint"
	dataKey        = "value"
)

// Provider implements console.SecretStore against the Kubernetes API.
type Provider struct {
	c         client.Client
	namespace func(orgID string) string
}

// New builds a kube secret provider. namespace maps an org id to the namespace
// its secrets live in (the tenancy track owns provisioning those namespaces).
func New(c client.Client, namespace func(orgID string) string) *Provider {
	return &Provider{c: c, namespace: namespace}
}

// objectName maps a logical secret name (which may contain characters invalid in
// a k8s object name, e.g. "OPENAI_API_KEY") to a deterministic DNS-1123-safe
// object name. The logical name is preserved in the annName annotation.
func objectName(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "ms-" + hex.EncodeToString(sum[:])[:32]
}

// Put creates or rotates the named secret in the org's namespace. The value is
// stored in the Secret; the returned view carries only metadata.
func (p *Provider) Put(ctx context.Context, orgID, name, value string) (console.SecretView, error) {
	ns := p.namespace(orgID)
	key := client.ObjectKey{Namespace: ns, Name: objectName(name)}

	version := 1
	existing := &corev1.Secret{}
	switch err := p.c.Get(ctx, key, existing); {
	case err == nil:
		if v, perr := strconv.Atoi(existing.Annotations[annVersion]); perr == nil {
			version = v + 1
		}
	case apierrors.IsNotFound(err):
		existing = nil
	default:
		return console.SecretView{}, fmt.Errorf("get secret: %w", err)
	}

	fp := console.Fingerprint(value)
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: ns,
			Labels: map[string]string{
				labelManagedBy: managedByValue,
				labelOrg:       orgID,
			},
			Annotations: map[string]string{
				annName:        name,
				annVersion:     strconv.Itoa(version),
				annFingerprint: fp,
			},
		},
		Data: map[string][]byte{dataKey: []byte(value)},
	}

	if existing == nil {
		if err := p.c.Create(ctx, sec); err != nil {
			return console.SecretView{}, fmt.Errorf("create secret: %w", err)
		}
	} else {
		sec.ResourceVersion = existing.ResourceVersion
		if err := p.c.Update(ctx, sec); err != nil {
			return console.SecretView{}, fmt.Errorf("update secret: %w", err)
		}
	}
	return console.SecretView{Name: name, OrgID: orgID, Provider: "kube", Mode: "copy_in", Version: version, Fingerprint: fp}, nil
}

// List returns the org's secrets (metadata only) from the org's namespace.
func (p *Provider) List(ctx context.Context, orgID string) ([]console.SecretView, error) {
	var secrets corev1.SecretList
	if err := p.c.List(ctx, &secrets,
		client.InNamespace(p.namespace(orgID)),
		client.MatchingLabels{labelManagedBy: managedByValue, labelOrg: orgID},
	); err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	out := make([]console.SecretView, 0, len(secrets.Items))
	for i := range secrets.Items {
		out = append(out, viewOf(&secrets.Items[i], orgID))
	}
	return out, nil
}

// Delete removes the org's named secret. A missing secret is console.ErrNotFound.
func (p *Provider) Delete(ctx context.Context, orgID, name string) error {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: p.namespace(orgID), Name: objectName(name)}}
	if err := p.c.Delete(ctx, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return console.ErrNotFound
		}
		return fmt.Errorf("delete secret: %w", err)
	}
	return nil
}

func viewOf(s *corev1.Secret, orgID string) console.SecretView {
	version, _ := strconv.Atoi(s.Annotations[annVersion])
	return console.SecretView{
		Name:        s.Annotations[annName],
		OrgID:       orgID,
		Provider:    "kube",
		Mode:        "copy_in",
		Version:     version,
		Fingerprint: s.Annotations[annFingerprint],
	}
}
