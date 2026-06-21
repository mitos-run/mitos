package agentcli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestTemplateBuildFromDockerfileDispatches asserts `mitos template build` parses
// a Dockerfile, prints the build plan, and dispatches Build with the parsed
// spec and template name.
func TestTemplateBuildFromDockerfileDispatches(t *testing.T) {
	df := writeTemp(t, "Dockerfile", "FROM python:3.12\nRUN pip install flask\nCMD [\"python\",\"app.py\"]\n")
	fb := NewFakeBackend()
	var out, errw bytes.Buffer
	code := Run(context.Background(), []string{"template", "build", "--dockerfile", df, "--name", "web"}, fb, &out, &errw)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errw.String())
	}
	tb := fb.TB
	if tb == nil || len(tb.Builds) != 1 {
		t.Fatalf("expected 1 build dispatch, got %+v", tb)
	}
	if tb.Builds[0].Name != "web" {
		t.Errorf("build name = %q, want web", tb.Builds[0].Name)
	}
	if tb.Builds[0].Spec.Image != "python:3.12" {
		t.Errorf("build image = %q, want python:3.12", tb.Builds[0].Spec.Image)
	}
	if len(tb.Builds[0].Spec.BuildSteps) != 1 {
		t.Errorf("build steps = %+v, want 1", tb.Builds[0].Spec.BuildSteps)
	}
	// The build plan is printed so the caller sees which steps are cached.
	if !strings.Contains(out.String(), "step 0") {
		t.Errorf("expected a build plan in output, got %q", out.String())
	}
}

// TestTemplatePushDispatches asserts `mitos template push <name>` dispatches Push.
func TestTemplatePushDispatches(t *testing.T) {
	fb := NewFakeBackend()
	var out, errw bytes.Buffer
	code := Run(context.Background(), []string{"template", "push", "web"}, fb, &out, &errw)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errw.String())
	}
	if fb.TB == nil || len(fb.TB.Pushes) != 1 || fb.TB.Pushes[0] != "web" {
		t.Fatalf("expected push of web, got %+v", fb.TB)
	}
}

// TestTemplateBuildSurfacesTypedError asserts a backend build error is reported
// with its remediation, so a failing build returns a clear, actionable message.
func TestTemplateBuildSurfacesTypedError(t *testing.T) {
	df := writeTemp(t, "Dockerfile", "FROM alpine\nRUN false\n")
	fb := NewFakeBackend()
	fb.TB = &FakeTemplateBackend{BuildErr: errTemplateBuildBoom}
	var out, errw bytes.Buffer
	code := Run(context.Background(), []string{"template", "build", "--dockerfile", df, "--name", "x"}, fb, &out, &errw)
	if code == 0 {
		t.Fatal("expected nonzero exit on build error")
	}
	if !strings.Contains(errw.String(), "remediation") && !strings.Contains(errw.String(), "step") {
		t.Errorf("error output should surface the typed build error, got %q", errw.String())
	}
}

// TestTemplateBuildRequiresSource asserts build without --dockerfile or --spec is
// a usage error.
func TestTemplateBuildRequiresSource(t *testing.T) {
	fb := NewFakeBackend()
	var out, errw bytes.Buffer
	code := Run(context.Background(), []string{"template", "build", "--name", "x"}, fb, &out, &errw)
	if code != 2 {
		t.Fatalf("exit %d, want 2 (usage error)", code)
	}
}
