package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"mitos.run/mitos/internal/saas/quota"
)

// fakeCounter is a quota.LiveCounter returning a fixed result or error.
type fakeCounter struct {
	usage quota.LiveUsage
	err   error
}

func (f fakeCounter) Count(context.Context, string) (quota.LiveUsage, error) {
	return f.usage, f.err
}

// TestProbeLiveCounterLogsActionableErrorAndKeepsServing asserts the startup
// self-check surfaces a broken live counter (RBAC or scheme misconfiguration)
// at ERROR with remediation text, and returns instead of crashing: the
// per-request fail-closed path already protects admission, so the probe's job
// is to make a persistent deny-all-creates posture loudly visible at boot,
// never to take the gateway down on a transient blip.
func TestProbeLiveCounterLogsActionableErrorAndKeepsServing(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	probeLiveCounter(context.Background(), fakeCounter{err: errors.New("sandboxes.mitos.run is forbidden")}, log)

	out := buf.String()
	if !strings.Contains(out, "level=ERROR") {
		t.Fatalf("probe error not logged at ERROR: %q", out)
	}
	if !strings.Contains(out, "DENIED") {
		t.Errorf("probe error does not state the consequence (creates denied): %q", out)
	}
	if !strings.Contains(out, "remediation") || !strings.Contains(out, "RBAC") {
		t.Errorf("probe error carries no actionable remediation naming the misconfig class: %q", out)
	}
}

// TestProbeLiveCounterHealthyLogsOK asserts a healthy counter logs a normal
// startup line and nothing at error level.
func TestProbeLiveCounterHealthyLogsOK(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	probeLiveCounter(context.Background(), fakeCounter{}, log)

	out := buf.String()
	if strings.Contains(out, "level=ERROR") {
		t.Fatalf("healthy probe logged an error: %q", out)
	}
	if !strings.Contains(out, "self-check ok") {
		t.Errorf("healthy probe did not log the ok line: %q", out)
	}
}
