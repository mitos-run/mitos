package charttest

import (
	"strings"
	"testing"
)

// TestNoImagePullSecretsByDefault asserts a default install attaches NO
// imagePullSecrets to any workload pod. The published mitos-* images are public
// on GHCR, so a fresh install pulls without a credential; referencing a
// ghcr-pull secret that does not exist would only emit a confusing
// "Unable to retrieve some image pull secrets" warning event on every pod.
// Issue #399.
func TestNoImagePullSecretsByDefault(t *testing.T) {
	out := render(t)
	if strings.Contains(out, "imagePullSecrets:") {
		t.Fatalf("default render attaches imagePullSecrets, but the images are public; expected none\n%s", out)
	}
}

// TestImagePullSecretsRenderWhenSet asserts the knob still works for a private
// mirror: setting imagePullSecrets[0].name attaches that pull secret to the
// workload pods. This is the only configuration that needs it.
func TestImagePullSecretsRenderWhenSet(t *testing.T) {
	out := render(t, "imagePullSecrets[0].name=ghcr-pull")
	mustContain(t, out, "imagePullSecrets:")
	mustContain(t, out, "- name: ghcr-pull")
}

// TestImagePullSecretNotRenderedByDefault asserts the chart does not render the
// dockerconfigjson Secret by default (imagePullSecret.create=false): public
// images need no credential.
func TestImagePullSecretNotRenderedByDefault(t *testing.T) {
	out := render(t)
	if strings.Contains(out, "kubernetes.io/dockerconfigjson") {
		t.Fatalf("default render created a dockerconfigjson pull Secret; public images need none\n%s", out)
	}
}
