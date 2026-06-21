package v1alpha1

import (
	"reflect"
	"strings"
	"testing"
)

// TestInitCommandsReturnsLegacyInitWhenNoBuildSteps asserts that a template that
// sets only the legacy Init list builds exactly those commands, so the
// BuildSteps addition does not change existing templates.
func TestInitCommandsReturnsLegacyInitWhenNoBuildSteps(t *testing.T) {
	s := SandboxTemplateSpec{
		Image: "python:3.12",
		Init:  []string{"pip install -r requirements.txt", "python -m compileall ."},
	}
	got := s.InitCommands()
	want := []string{"pip install -r requirements.txt", "python -m compileall ."}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("InitCommands() = %v, want %v", got, want)
	}
}

// TestInitCommandsFlattensBuildStepsInOrder asserts run, env, and workdir steps
// flatten into ordered in-VM commands and copy steps contribute none.
func TestInitCommandsFlattensBuildStepsInOrder(t *testing.T) {
	s := SandboxTemplateSpec{
		Image: "node:24",
		BuildSteps: []BuildStep{
			{Type: BuildStepWorkdir, Workdir: "/app"},
			{Type: BuildStepCopy, Source: "app/", Dest: "/app"},
			{Type: BuildStepEnv, EnvName: "NODE_ENV", EnvValue: "production"},
			{Type: BuildStepRun, Run: "npm ci"},
		},
	}
	got := s.InitCommands()
	if len(got) != 3 {
		t.Fatalf("InitCommands() returned %d commands, want 3 (copy contributes none): %v", len(got), got)
	}
	if !strings.Contains(got[0], "cd '/app'") {
		t.Errorf("first command should cd into the workdir, got %q", got[0])
	}
	if !strings.Contains(got[1], "export NODE_ENV='production'") {
		t.Errorf("env command should export the variable, got %q", got[1])
	}
	if !strings.Contains(got[1], "/etc/profile") {
		t.Errorf("env command should persist into /etc/profile so forks inherit it, got %q", got[1])
	}
	if got[2] != "npm ci" {
		t.Errorf("run command = %q, want %q", got[2], "npm ci")
	}
}

// TestInitCommandsBuildStepsTakePrecedenceOverInit asserts that when both are
// set BuildSteps wins, so the code-first form is authoritative.
func TestInitCommandsBuildStepsTakePrecedenceOverInit(t *testing.T) {
	s := SandboxTemplateSpec{
		Image:      "busybox",
		Init:       []string{"echo legacy"},
		BuildSteps: []BuildStep{{Type: BuildStepRun, Run: "echo declarative"}},
	}
	got := s.InitCommands()
	if !reflect.DeepEqual(got, []string{"echo declarative"}) {
		t.Fatalf("InitCommands() = %v, want [echo declarative]", got)
	}
}
