package sandboxtable

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "mitos.run/mitos/api/v1"
)

func mkForkSandbox(name, source string, replicas, ready int32) v1.Sandbox {
	return v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1.SandboxSpec{
			Source: v1.SandboxSource{
				FromSandbox: &v1.FromSandboxSource{Name: source},
			},
			Replicas: replicas,
		},
		Status: v1.SandboxStatus{ReadyReplicas: ready},
	}
}

func TestBuildLineageMultiLevelChainAndSiblings(t *testing.T) {
	now := metav1.Now().Time
	// root is a pool-sourced sandbox; fork-a and fork-b are children of root;
	// fork-a1 is a child of fork-a (multi-level chain).
	sandboxes := []v1.Sandbox{
		mkSandbox("root", "default", "web", v1.SandboxReady, "node-1", "ep", 0, now),
		mkForkSandbox("fork-b", "root", 1, 1),
		mkForkSandbox("fork-a", "root", 2, 2),
		mkForkSandbox("fork-a1", "fork-a", 1, 0),
	}
	roots := BuildLineage(sandboxes)
	if len(roots) != 1 {
		t.Fatalf("expected 1 root, got %d", len(roots))
	}
	r := roots[0]
	if r.Name != "root" || r.Kind != "sandbox" {
		t.Fatalf("root = %q/%q, want root/sandbox", r.Name, r.Kind)
	}
	if len(r.Children) != 2 {
		t.Fatalf("root should have 2 children, got %d", len(r.Children))
	}
	// Sorted by name: fork-a before fork-b.
	if r.Children[0].Name != "fork-a" || r.Children[1].Name != "fork-b" {
		t.Fatalf("children = %q,%q, want fork-a,fork-b", r.Children[0].Name, r.Children[1].Name)
	}
	if len(r.Children[0].Children) != 1 || r.Children[0].Children[0].Name != "fork-a1" {
		t.Fatalf("fork-a should have child fork-a1, got %+v", r.Children[0].Children)
	}
}

func TestFormatLineageRendersIndentedTree(t *testing.T) {
	now := metav1.Now().Time
	sandboxes := []v1.Sandbox{
		mkSandbox("root", "default", "web", v1.SandboxReady, "node-1", "ep", 0, now),
		mkForkSandbox("fork-a", "root", 2, 2),
		mkForkSandbox("fork-b", "root", 1, 0),
		mkForkSandbox("fork-a1", "fork-a", 1, 1),
	}
	out := FormatLineage(BuildLineage(sandboxes))
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d:\n%s", len(lines), out)
	}
	// Root has no glyph; children are indented under it.
	if !strings.HasPrefix(lines[0], "root") {
		t.Errorf("line 0 should start with root: %q", lines[0])
	}
	if !strings.Contains(lines[0], "sandbox") || !strings.Contains(lines[0], "node-1") {
		t.Errorf("root line should carry kind+node: %q", lines[0])
	}
	// fork-a is a non-last child, so a tee glyph; fork-a1 nests one level deeper.
	if !strings.Contains(lines[1], "fork-a") || !strings.Contains(lines[1], "|--") {
		t.Errorf("fork-a line should be a tee branch: %q", lines[1])
	}
	if !strings.Contains(lines[2], "fork-a1") {
		t.Errorf("line 2 should be fork-a1: %q", lines[2])
	}
	// fork-a1 indentation is deeper than fork-a.
	if indent(lines[2]) <= indent(lines[1]) {
		t.Errorf("fork-a1 should indent deeper than fork-a: %q vs %q", lines[2], lines[1])
	}
	// fork-b is the last child of root, so a corner glyph.
	if !strings.Contains(lines[3], "fork-b") || !strings.Contains(lines[3], "`--") {
		t.Errorf("fork-b line should be a corner branch: %q", lines[3])
	}
}

func TestFormatLineageMissingPhaseAndNodeAreDashes(t *testing.T) {
	// An orphan fork-sourced sandbox (source not in the supplied list) surfaces
	// as a root with dashes for phase+node when status is empty.
	sandboxes := []v1.Sandbox{mkForkSandbox("lonely", "absent-source", 1, 0)}
	out := FormatLineage(BuildLineage(sandboxes))
	// Orphan fork with no ready replicas and no node: phase + node are dashes.
	if !strings.Contains(out, "lonely") {
		t.Fatalf("orphan fork should appear: %q", out)
	}
	fields := strings.Fields(out)
	// fields: lonely fork - -
	if len(fields) < 4 || fields[2] != "-" || fields[3] != "-" {
		t.Errorf("missing phase/node should be dashes, got fields %v", fields)
	}
}

func TestFormatLineageEmpty(t *testing.T) {
	out := FormatLineage(BuildLineage(nil))
	if !strings.Contains(out, "No sandboxes found") {
		t.Errorf("empty lineage should report no sandboxes, got %q", out)
	}
}

func indent(line string) int {
	return len(line) - len(strings.TrimLeft(line, " |`-"))
}
