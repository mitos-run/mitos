package runmanifest

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "mitos.run/mitos/api/v1"
)

// PoolName is the SandboxPool name for this manifest (resources.pool override, or
// the manifest name).
func (m *Manifest) PoolName() string {
	if m.Resources.Pool != "" {
		return m.Resources.Pool
	}
	return m.Name
}

// GoldenPool maps the manifest to the golden v1.SandboxPool that instances fork
// from. It bakes only NON-secret config: the image, the (workdir-wrapped) command,
// non-secret env, and resources. Secret values are injected per-fork by the
// provisioner and never appear here, so the golden snapshot is shareable without
// leaking any clicker's keys (fork-correctness secret non-inheritance).
//
// Build-from-source goldens (source.build) resolve to a built image in a later
// slice; until then GoldenPool requires source.image and returns an actionable
// error otherwise. The HTTP ready gate is preserved on the manifest for the
// snapshot-after-serving slice; this mapping uses the existing SnapshotAfterReady
// trigger.
func (m *Manifest) GoldenPool(namespace string) (*v1.SandboxPool, error) {
	if m.Source.Image == "" {
		return nil, fmt.Errorf("mitos.yaml %q: GoldenPool needs source.image; build-from-source golden resolution is a later slice", m.Name)
	}
	res, err := m.sandboxResources()
	if err != nil {
		return nil, err
	}
	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.PoolName(),
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "run-with-mitos",
				"mitos.run/run-manifest":       m.Name,
			},
		},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{
				Image:     m.Source.Image,
				Command:   m.effectiveCommand(),
				Env:       m.nonSecretEnv(),
				Resources: res,
				Network:   m.egressPolicy(),
			},
			Snapshots: &v1.PoolSnapshots{
				ReplicasPerNode: 1,
				SnapshotAfter:   v1.SnapshotAfterReady,
			},
			Warm: &v1.PoolWarm{Min: 1},
		},
	}
	return pool, nil
}

// effectiveCommand returns the command to run in the guest, honoring run.workdir by
// wrapping in a shell when a workdir is set. With no command the image entrypoint
// is inherited (nil).
func (m *Manifest) effectiveCommand() []string {
	if len(m.Run.Command) == 0 {
		return nil
	}
	if m.Run.Workdir == "" {
		return append([]string(nil), m.Run.Command...)
	}
	quoted := make([]string, len(m.Run.Command))
	for i, c := range m.Run.Command {
		quoted[i] = shellQuote(c)
	}
	return []string{"sh", "-c", fmt.Sprintf("cd %s && exec %s", shellQuote(m.Run.Workdir), strings.Join(quoted, " "))}
}

// nonSecretEnv maps run.env to a deterministically ordered []corev1.EnvVar. Secret
// values are NOT here; they are injected per-fork.
func (m *Manifest) nonSecretEnv() []corev1.EnvVar {
	if len(m.Run.Env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m.Run.Env))
	for k := range m.Run.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]corev1.EnvVar, 0, len(keys))
	for _, k := range keys {
		out = append(out, corev1.EnvVar{Name: k, Value: m.Run.Env[k]})
	}
	return out
}

// sandboxResources parses the manifest CPU and memory into a SandboxResources.
// Validation already verified they parse; this re-parses to build the typed value.
func (m *Manifest) sandboxResources() (v1.SandboxResources, error) {
	var r v1.SandboxResources
	if m.Resources.CPU != "" {
		q, err := resource.ParseQuantity(m.Resources.CPU)
		if err != nil {
			return r, fmt.Errorf("mitos.yaml %q: resources.cpu: %w", m.Name, err)
		}
		r.CPU = q
	}
	if m.Resources.Memory != "" {
		q, err := resource.ParseQuantity(m.Resources.Memory)
		if err != nil {
			return r, fmt.Errorf("mitos.yaml %q: resources.memory: %w", m.Name, err)
		}
		r.Memory = q
	}
	return r, nil
}

// egressPolicy turns the manifest egress allowlist into a default-deny pool
// network policy: deny everything, allow only the declared destinations. An empty
// allowlist leaves the pool default in place (nil) rather than block all egress,
// since some apps declare egress per-fork; the provisioner can still tighten it.
func (m *Manifest) egressPolicy() *v1.NetworkPolicy {
	if len(m.Egress.Allow) == 0 {
		return nil
	}
	return &v1.NetworkPolicy{
		Egress: v1.EgressDeny,
		Allow:  append([]string(nil), m.Egress.Allow...),
	}
}

// RequiredSecretNames returns the names of secrets the clicker must supply (those
// not mintable via generate). The provisioner uses this to drive the consent
// screen and to fail closed when a required secret is missing.
func (m *Manifest) RequiredSecretNames() []string {
	var out []string
	for _, s := range m.Secrets {
		if s.Required {
			out = append(out, s.Name)
		}
	}
	return out
}

// shellQuote single-quotes a string for safe use inside an sh -c command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
