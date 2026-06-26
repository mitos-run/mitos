package runmanifest

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "mitos.run/mitos/api/v1"
)

// ProvisionResult is what one click produces: a per-fork Secret holding the
// clicker's values, and the Sandbox that forks the golden and consumes them. The
// caller applies both. Secret VALUES live only in Result.Secret.Data; this package
// never logs them.
type ProvisionResult struct {
	Secret  *corev1.Secret
	Sandbox *v1.Sandbox
}

// randSource is the entropy used for generated secrets; overridable in tests.
var randSource = rand.Read

// Provision maps a manifest plus the clicker-supplied secret values into a
// per-fork Sandbox (forking the golden pool) and the Secret that backs it.
//
// instanceLabel is the single DNS label for this instance: the expose subdomain
// (label.<expose-domain>), the Secret/Sandbox/Workspace names. Required secrets
// that are neither supplied nor mintable fail closed. Mintable secrets
// (generate > 0) left blank are minted from crypto/rand. SecretInheritance is set
// to reissue so a fork never inherits the golden's in-memory secrets.
func Provision(m *Manifest, supplied map[string]string, namespace, instanceLabel string) (*ProvisionResult, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if !dnsLabel.MatchString(instanceLabel) {
		return nil, fmt.Errorf("instance label %q must be a DNS label", instanceLabel)
	}

	values := map[string][]byte{}
	var mounts []v1.SecretMount
	secretName := instanceLabel + "-secrets"
	for _, s := range m.Secrets {
		v, ok := supplied[s.Name]
		switch {
		case ok && v != "":
			values[s.Name] = []byte(v)
		case s.Generate > 0:
			minted, err := generateSecret(s.Generate)
			if err != nil {
				return nil, fmt.Errorf("mint secret %q: %w", s.Name, err)
			}
			values[s.Name] = []byte(minted)
		case s.Required:
			return nil, fmt.Errorf("secret %q is required but was not provided", s.Name)
		default:
			continue // optional, not supplied: skip it.
		}
		mounts = append(mounts, v1.SecretMount{
			Name:      s.Name,
			SecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secretName}, Key: s.Name},
			EnvVar:    s.Name,
		})
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "run-with-mitos",
				"mitos.run/run-manifest":       m.Name,
				"mitos.run/instance":           instanceLabel,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: values,
	}

	sandbox := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instanceLabel,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "run-with-mitos",
				"mitos.run/run-manifest":       m.Name,
				"mitos.run/instance":           instanceLabel,
			},
		},
		Spec: v1.SandboxSpec{
			Source:            v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: m.PoolName()}},
			Secrets:           mounts,
			SecretInheritance: v1.SecretReissue,
			Expose: &v1.SandboxExpose{
				Port:    int32(m.Preview.Port),
				Label:   instanceLabel,
				Sharing: exposeSharing(m.Preview.Auth),
			},
		},
	}
	if m.Workspace != nil && m.Workspace.Persist {
		sandbox.Spec.WorkspaceRef = &v1.LocalObjectReference{Name: instanceLabel + "-workspace"}
	}

	return &ProvisionResult{Secret: secret, Sandbox: sandbox}, nil
}

// exposeSharing maps the manifest preview.auth to the expose sharing tier. The
// auth ladder requires a verified identity; the default is owner-private.
func exposeSharing(auth string) string {
	switch auth {
	case "ladder", "required":
		return "authenticated"
	default:
		return "private"
	}
}

// generateSecret returns a hex string carrying n bytes of entropy.
func generateSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := randSource(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
