package runmanifest

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"

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
//
// publicURL is this instance's resolved public URL (https://<instanceLabel>.<expose
// -domain>). It is delivered to the fork as the MITOS_PUBLIC_URL env var, and any
// run.env value referencing ${MITOS_PUBLIC_URL} is resolved per-fork into the
// Sandbox env (overlaying the shared golden's value), so this fork's exec sessions
// and runtime reads see its own URL (issue #476). An empty publicURL injects
// nothing. publicURL is a non-secret URL, never a credential.
func Provision(m *Manifest, supplied map[string]string, namespace, instanceLabel, publicURL string) (*ProvisionResult, error) {
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
	sandbox.Spec.Env = publicURLEnv(m, publicURL)

	return &ProvisionResult{Secret: secret, Sandbox: sandbox}, nil
}

// publicURLEnv builds the per-fork env that carries this instance's public URL:
// MITOS_PUBLIC_URL set to publicURL, plus every run.env entry that references
// ${MITOS_PUBLIC_URL} resolved to publicURL (so a fork's exec sessions see this
// instance's own URL, overlaying the shared golden's value). Entries that do not
// reference the URL are left in the golden only and not duplicated here. Returns
// nil when publicURL is empty, so a caller without a resolved URL provisions
// exactly as before.
func publicURLEnv(m *Manifest, publicURL string) []corev1.EnvVar {
	if publicURL == "" {
		return nil
	}
	keys := make([]string, 0, len(m.Run.Env))
	for k, v := range m.Run.Env {
		if k != PublicURLEnvVar && referencesPublicURL(v) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]corev1.EnvVar, 0, len(keys)+1)
	out = append(out, corev1.EnvVar{Name: PublicURLEnvVar, Value: publicURL})
	for _, k := range keys {
		out = append(out, corev1.EnvVar{Name: k, Value: expandPublicURL(m.Run.Env[k], publicURL)})
	}
	return out
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
