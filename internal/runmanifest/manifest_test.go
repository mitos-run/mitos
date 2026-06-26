package runmanifest

import (
	"os"
	"strings"
	"testing"
)

func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

// TestParseExamples asserts every shipped example manifest parses and validates.
func TestParseExamples(t *testing.T) {
	for _, f := range []string{"openclaw.yaml", "deerflow.yaml", "hermes.yaml"} {
		if _, err := Parse(mustRead(t, f)); err != nil {
			t.Errorf("%s: Parse: %v", f, err)
		}
	}
}

// TestGoldenPoolOpenClaw asserts the image-based manifest maps to a golden pool
// with the workdir-wrapped command, non-secret env, resources, the ready snapshot
// trigger, and a warm floor.
func TestGoldenPoolOpenClaw(t *testing.T) {
	m, err := Parse(mustRead(t, "openclaw.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	pool, err := m.GoldenPool("sandboxes")
	if err != nil {
		t.Fatalf("GoldenPool: %v", err)
	}
	if pool.Name != "openclaw" || pool.Namespace != "sandboxes" {
		t.Fatalf("pool identity = %s/%s, want openclaw/sandboxes", pool.Namespace, pool.Name)
	}
	tpl := pool.Spec.Template
	if tpl == nil {
		t.Fatal("template is nil")
	}
	if tpl.Image != "ghcr.io/openclaw/openclaw:latest" {
		t.Errorf("image = %q", tpl.Image)
	}
	// workdir set -> command wrapped in sh -c with cd and exec.
	if len(tpl.Command) != 3 || tpl.Command[0] != "sh" || tpl.Command[1] != "-c" {
		t.Fatalf("command not shell-wrapped: %v", tpl.Command)
	}
	if !strings.Contains(tpl.Command[2], "cd '/app' && exec 'node' 'openclaw.mjs'") {
		t.Errorf("wrapped command = %q", tpl.Command[2])
	}
	if tpl.Resources.CPU.String() != "2" || tpl.Resources.Memory.String() != "2Gi" {
		t.Errorf("resources = %s/%s", tpl.Resources.CPU.String(), tpl.Resources.Memory.String())
	}
	if pool.Spec.Snapshots == nil || pool.Spec.Snapshots.SnapshotAfter != "Ready" {
		t.Errorf("snapshot trigger = %+v", pool.Spec.Snapshots)
	}
	if pool.Spec.Warm == nil || pool.Spec.Warm.Min != 1 {
		t.Errorf("warm = %+v", pool.Spec.Warm)
	}
	// Non-secret env carries HOME but never a secret name.
	var sawHome bool
	for _, e := range tpl.Env {
		if e.Name == "HOME" && e.Value == "/home/node" {
			sawHome = true
		}
	}
	if !sawHome {
		t.Error("non-secret env HOME missing from golden")
	}
}

// TestSecretValuesNeverInGolden is the load-bearing security property: declared
// secrets must NOT appear in the golden snapshot's env (they are injected
// per-fork). The golden is shareable precisely because it holds no clicker keys.
func TestSecretValuesNeverInGolden(t *testing.T) {
	m, err := Parse(mustRead(t, "openclaw.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	pool, err := m.GoldenPool("sandboxes")
	if err != nil {
		t.Fatalf("GoldenPool: %v", err)
	}
	secretNames := map[string]bool{}
	for _, s := range m.Secrets {
		secretNames[s.Name] = true
	}
	if len(secretNames) == 0 {
		t.Fatal("fixture has no secrets to check")
	}
	for _, e := range pool.Spec.Template.Env {
		if secretNames[e.Name] {
			t.Errorf("secret %q leaked into the golden snapshot env", e.Name)
		}
	}
	// And the required-secret set is what the consent screen would prompt for.
	if got := m.RequiredSecretNames(); len(got) != 1 || got[0] != "ANTHROPIC_API_KEY" {
		t.Errorf("required secrets = %v, want [ANTHROPIC_API_KEY]", got)
	}
}

// TestGoldenPoolTrackAnnotations asserts the source.track policy is stamped onto
// the golden pool as annotations for the auto-update reconciler to read.
func TestGoldenPoolTrackAnnotations(t *testing.T) {
	m, err := Parse(mustRead(t, "openclaw.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	pool, err := m.GoldenPool("ns")
	if err != nil {
		t.Fatalf("GoldenPool: %v", err)
	}
	ann := pool.Annotations
	if ann[AnnTrackWatch] != "ghcr.io/openclaw/openclaw" {
		t.Errorf("track-watch = %q", ann[AnnTrackWatch])
	}
	if ann[AnnTrackChannel] != "latest" {
		t.Errorf("track-channel = %q", ann[AnnTrackChannel])
	}
	if ann[AnnTrackAction] != "resnapshot+offer-rebase" {
		t.Errorf("track-action = %q", ann[AnnTrackAction])
	}
	if ann[AnnResolvedImage] != "ghcr.io/openclaw/openclaw:latest" {
		t.Errorf("resolved-image = %q", ann[AnnResolvedImage])
	}
}

// TestGoldenPoolBuildNotYet asserts a build-from-source manifest parses but
// GoldenPool fails loudly until that slice lands (no silent half-mapping).
func TestGoldenPoolBuildNotYet(t *testing.T) {
	m, err := Parse(mustRead(t, "deerflow.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := m.GoldenPool("sandboxes"); err == nil {
		t.Fatal("GoldenPool on a build-from-source manifest should error until that slice lands")
	} else if !strings.Contains(err.Error(), "source.image") {
		t.Errorf("error should name source.image, got: %v", err)
	}
}

// TestValidateErrors covers the actionable failure modes.
func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"missing name", "source: {image: x}\npreview: {port: 80}", "name is required"},
		{"bad name", "name: Bad_Name\nsource: {image: x}\npreview: {port: 80}", "DNS label"},
		{"image and build", "name: a\nsource: {image: x, build: {repo: r}}\npreview: {port: 80}", "exactly one of image or build"},
		{"no source", "name: a\nsource: {}\npreview: {port: 80}", "requires an image"},
		{"no preview port", "name: a\nsource: {image: x}\npreview: {}", "preview.port is required"},
		{"bad preview auth", "name: a\nsource: {image: x}\npreview: {port: 80, auth: open}", "preview.auth"},
		{"dup secret", "name: a\nsource: {image: x}\npreview: {port: 80}\nsecrets: [{name: K}, {name: K}]", "more than once"},
		{"bad cpu", "name: a\nsource: {image: x}\npreview: {port: 80}\nresources: {cpu: two}", "resources.cpu"},
		{"bad track action", "name: a\nsource: {image: x, track: {watch: w, on_new_release: yolo}}\npreview: {port: 80}", "on_new_release"},
		{"unknown field", "name: a\nsource: {image: x}\npreview: {port: 80}\nbogus: 1", "parse"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.yaml))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want substring %q", err.Error(), c.want)
			}
		})
	}
}
