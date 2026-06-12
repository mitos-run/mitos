package workspace

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requireGit skips the test when git is not on PATH, so the unit suite is not
// flaky on a minimal image. CI's linux runner has git.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH; skipping git rendezvous test")
	}
}

// gitOut runs git in dir and returns trimmed stdout, failing the test on error.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestRenderBranch(t *testing.T) {
	got, err := RenderBranch("attempt/{{.name}}", "agent-7f3a")
	if err != nil {
		t.Fatal(err)
	}
	if got != "attempt/agent-7f3a" {
		t.Fatalf("RenderBranch = %q, want attempt/agent-7f3a", got)
	}
	// An empty template falls back to the claim name on a deterministic prefix.
	got, err = RenderBranch("", "agent-7f3a")
	if err != nil {
		t.Fatal(err)
	}
	if got != "attempt/agent-7f3a" {
		t.Fatalf("RenderBranch empty template = %q, want attempt/agent-7f3a", got)
	}
}

func TestRendezvousPushesToLocalBareRepo(t *testing.T) {
	requireGit(t)
	ctx := context.Background()

	// A local bare repo stands in for the rendezvous remote.
	bare := filepath.Join(t.TempDir(), "rendezvous.git")
	gitOut(t, t.TempDir(), "init", "--bare", bare)

	repoFiles := map[string]string{
		"repo/main.go":   "package main\n",
		"repo/README.md": "# attempt\n",
	}
	branch := "attempt/agent-7f3a"
	if err := Rendezvous(ctx, repoFiles, bare, branch); err != nil {
		t.Fatalf("Rendezvous: %v", err)
	}

	// The branch must exist on the remote and carry exactly one commit.
	refs := gitOut(t, bare, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if !strings.Contains(refs, branch) {
		t.Fatalf("remote refs %q missing branch %q", refs, branch)
	}
	count := gitOut(t, bare, "rev-list", "--count", branch)
	if count != "1" {
		t.Fatalf("branch %q has %s commits, want 1", branch, count)
	}

	// The pushed content must match the repo paths (the workspace-relative names
	// are preserved on the remote tree).
	tree := gitOut(t, bare, "ls-tree", "-r", "--name-only", branch)
	for name := range repoFiles {
		if !strings.Contains(tree, name) {
			t.Fatalf("pushed tree %q missing %q", tree, name)
		}
	}
	got := gitOut(t, bare, "show", branch+":repo/main.go")
	if got != "package main" {
		t.Fatalf("pushed repo/main.go = %q, want the source content", got)
	}
}

func TestRendezvousNoFilesIsNoOp(t *testing.T) {
	requireGit(t)
	// No repo files: a {git} output with nothing to push is a no-op, not an error.
	if err := Rendezvous(context.Background(), nil, "unused-remote", "attempt/x"); err != nil {
		t.Fatalf("empty Rendezvous should be a no-op, got %v", err)
	}
}
