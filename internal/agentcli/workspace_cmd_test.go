package agentcli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestWsCreateDispatches(t *testing.T) {
	fb := NewFakeWorkspaceBackend()
	var out, errw bytes.Buffer
	code := runWs(context.Background(), []string{"create", "proj-x"}, fb, &out, &errw)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errw.String())
	}
	calls := fb.RecordedCalls()
	if len(calls) != 1 || calls[0].Method != "ws_create" || calls[0].SandboxID != "proj-x" {
		t.Fatalf("unexpected calls: %+v", calls)
	}
}

func TestWsLogRendersRevisions(t *testing.T) {
	fb := NewFakeWorkspaceBackend()
	fb.Revisions = []RevisionInfo{{Name: "proj-x-2", Phase: "Committed", Lineage: "fromClaim:c2"}}
	var out, errw bytes.Buffer
	if code := runWs(context.Background(), []string{"log", "proj-x"}, fb, &out, &errw); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "proj-x-2") || !strings.Contains(out.String(), "fromClaim:c2") {
		t.Fatalf("log output missing revision: %s", out.String())
	}
}

func TestWsForkPrintsNewRevision(t *testing.T) {
	fb := NewFakeWorkspaceBackend()
	fb.NewRev = "branch-7"
	var out, errw bytes.Buffer
	code := runWs(context.Background(), []string{"fork", "proj-x", "proj-x-2", "branch"}, fb, &out, &errw)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errw.String())
	}
	if !strings.Contains(out.String(), "branch-7") {
		t.Fatalf("fork did not print new revision: %s", out.String())
	}
}

func TestWsUnknownSubcommandUsageError(t *testing.T) {
	var out, errw bytes.Buffer
	if code := runWs(context.Background(), []string{"frobnicate"}, NewFakeWorkspaceBackend(), &out, &errw); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

func TestCmdServeSuccess(t *testing.T) {
	fb := NewFakeWorkspaceBackend()
	fb.ServeResult = ServeResult{
		SandboxName: "sbx-abc123",
		Label:       "myws",
		URL:         "https://myws.mitos.app/",
		Sharing:     "private",
	}
	var out, errw bytes.Buffer
	code := runWs(context.Background(), []string{"serve", "myws", "--pool", "python", "--expose-domain", "mitos.app"}, fb, &out, &errw)
	if code != 0 {
		t.Fatalf("exit %d, stderr=%s", code, errw.String())
	}
	if !strings.Contains(out.String(), "https://myws.mitos.app/") {
		t.Fatalf("output does not contain URL: %s", out.String())
	}
}

func TestCmdServeMissingPool(t *testing.T) {
	fb := NewFakeWorkspaceBackend()
	var out, errw bytes.Buffer
	code := runWs(context.Background(), []string{"serve", "myws", "--expose-domain", "mitos.app"}, fb, &out, &errw)
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(errw.String(), "pool") {
		t.Fatalf("errw = %q, want containing 'pool'", errw.String())
	}
}

func TestCmdServeMissingDomain(t *testing.T) {
	t.Setenv("MITOS_EXPOSE_DOMAIN", "")
	fb := NewFakeWorkspaceBackend()
	var out, errw bytes.Buffer
	code := runWs(context.Background(), []string{"serve", "myws", "--pool", "python"}, fb, &out, &errw)
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(errw.String(), "expose-domain") {
		t.Fatalf("errw = %q, want containing 'expose-domain'", errw.String())
	}
}
