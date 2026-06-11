package agentcli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// recordingRunner records every argv it is asked to run and optionally returns a
// canned error for the call whose argv contains a given substring.
type recordingRunner struct {
	calls   [][]string
	failOn  string
	failErr error
}

func (r *recordingRunner) run(_ context.Context, argv []string) error {
	r.calls = append(r.calls, argv)
	if r.failOn != "" && strings.Contains(strings.Join(argv, " "), r.failOn) {
		return r.failErr
	}
	return nil
}

func joinedCalls(calls [][]string) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = strings.Join(c, " ")
	}
	return out
}

func TestDevUpSequence(t *testing.T) {
	rr := &recordingRunner{}
	var out strings.Builder
	err := DevUp(context.Background(), DevOptions{}, rr.run, &out)
	if err != nil {
		t.Fatalf("DevUp: %v", err)
	}
	got := joinedCalls(rr.calls)
	if len(got) < 4 {
		t.Fatalf("want at least 4 commands, got %d: %v", len(got), got)
	}
	// First command must create the kind cluster with the configured name and
	// the kind config file.
	if !strings.HasPrefix(got[0], "kind create cluster") {
		t.Fatalf("first command = %q, want 'kind create cluster ...'", got[0])
	}
	if !strings.Contains(got[0], "--name "+defaultClusterName) {
		t.Fatalf("kind create missing cluster name: %q", got[0])
	}
	if !strings.Contains(got[0], "hack/kind-config.yaml") {
		t.Fatalf("kind create missing config: %q", got[0])
	}
	// CRDs and controller are applied.
	all := strings.Join(got, "\n")
	if !strings.Contains(all, "kubectl apply -f deploy/crds/") {
		t.Fatalf("want CRDs applied, got:\n%s", all)
	}
	if !strings.Contains(all, "deploy/controller/") {
		t.Fatalf("want controller applied, got:\n%s", all)
	}
	// A pool is applied (the inline mock-friendly pool written to a temp file).
	poolApplied := strings.Contains(all, "agentrun-dev-pool") ||
		strings.Contains(all, "python-pool.yaml") ||
		(strings.Contains(all, "kubectl apply") && strings.Contains(all, "SandboxPool"))
	if !poolApplied {
		t.Fatalf("want a default pool applied, got:\n%s", all)
	}
	// A clear mock-engine note is printed.
	if !strings.Contains(strings.ToLower(out.String()), "mock") {
		t.Fatalf("DevUp output = %q, want a mock-engine note", out.String())
	}
}

func TestDevUpToleratesExistingCluster(t *testing.T) {
	rr := &recordingRunner{failOn: "kind create cluster", failErr: errors.New("node(s) already exist for a cluster with the name")}
	var out strings.Builder
	err := DevUp(context.Background(), DevOptions{}, rr.run, &out)
	if err != nil {
		t.Fatalf("DevUp should tolerate an existing cluster, got: %v", err)
	}
	// It must continue past the create to apply the CRDs.
	all := strings.Join(joinedCalls(rr.calls), "\n")
	if !strings.Contains(all, "kubectl apply -f deploy/crds/") {
		t.Fatalf("want it to continue to apply CRDs after existing cluster, got:\n%s", all)
	}
}

func TestDevUpFailsOnApplyError(t *testing.T) {
	rr := &recordingRunner{failOn: "deploy/crds/", failErr: errors.New("apply boom")}
	var out strings.Builder
	err := DevUp(context.Background(), DevOptions{}, rr.run, &out)
	if err == nil {
		t.Fatalf("DevUp should fail when applying CRDs fails")
	}
}

func TestDevUpCustomClusterName(t *testing.T) {
	rr := &recordingRunner{}
	var out strings.Builder
	if err := DevUp(context.Background(), DevOptions{ClusterName: "mycluster"}, rr.run, &out); err != nil {
		t.Fatalf("DevUp: %v", err)
	}
	if !strings.Contains(joinedCalls(rr.calls)[0], "--name mycluster") {
		t.Fatalf("want custom cluster name, got: %q", joinedCalls(rr.calls)[0])
	}
}

func TestDevDownDeletesCluster(t *testing.T) {
	rr := &recordingRunner{}
	var out strings.Builder
	if err := DevDown(context.Background(), DevOptions{}, rr.run, &out); err != nil {
		t.Fatalf("DevDown: %v", err)
	}
	got := joinedCalls(rr.calls)
	if len(got) != 1 {
		t.Fatalf("DevDown should run exactly one command, got %d: %v", len(got), got)
	}
	if !strings.HasPrefix(got[0], "kind delete cluster") || !strings.Contains(got[0], "--name "+defaultClusterName) {
		t.Fatalf("DevDown command = %q, want 'kind delete cluster --name %s'", got[0], defaultClusterName)
	}
}
