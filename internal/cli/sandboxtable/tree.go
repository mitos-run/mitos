package sandboxtable

import (
	"sort"
	"strings"

	v1 "mitos.run/mitos/api/v1"
)

// TreeNode is one entry in the rendered sandbox/lineage DAG. A Sandbox with
// source.poolRef is a lineage root ("sandbox"); a Sandbox with
// source.fromSandbox is a child of whatever Sandbox its FromSandbox.Name names.
// Name is the object name, Phase is its lifecycle phase (empty renders as a
// dash), Node is the forkd node it landed on (empty renders as a dash), and
// Kind is "sandbox" or "fork" so the renderer can label the lineage roots.
type TreeNode struct {
	Name     string
	Kind     string
	Phase    string
	Node     string
	Children []*TreeNode
}

// BuildLineage assembles the parent->child lineage DAG from the cluster's
// Sandboxes. A Sandbox with source.poolRef is a lineage root; a Sandbox with
// source.fromSandbox is a child of whatever Sandbox its FromSandbox.Name names
// (a sandbox OR another fork, so a multi-level fork chain nests). Forks that
// name the same source are siblings. A fork whose source is not among the
// supplied objects is treated as its own root so it is never silently dropped.
// Output roots and children are sorted by name for a deterministic tree.
func BuildLineage(sandboxes []v1.Sandbox) []*TreeNode {
	nodes := make(map[string]*TreeNode)
	var roots []*TreeNode

	for i := range sandboxes {
		s := &sandboxes[i]
		kind := "sandbox"
		if s.Spec.Source.FromSandbox != nil {
			kind = "fork"
		}
		nodes[s.Name] = &TreeNode{
			Name:  s.Name,
			Kind:  kind,
			Phase: string(s.Status.Phase),
			Node:  s.Status.Node,
		}
	}

	// Link every fork-sourced sandbox under its source. Pool-sourced sandboxes
	// are always roots.
	for i := range sandboxes {
		s := &sandboxes[i]
		if s.Spec.Source.FromSandbox == nil {
			// Pool-sourced or revision-sourced: root.
			roots = append(roots, nodes[s.Name])
			continue
		}
		// Fork-sourced: child of the source sandbox.
		child := nodes[s.Name]
		sourceName := s.Spec.Source.FromSandbox.Name
		parent, ok := nodes[sourceName]
		if ok && sourceName != "" {
			parent.Children = append(parent.Children, child)
			continue
		}
		// Orphan: source not present in this view. Surface it as a root rather
		// than dropping it, so the operator still sees the object.
		roots = append(roots, child)
	}

	sortTree(roots)
	return roots
}

// sortTree sorts a node slice and every node's children by name, recursively,
// so the rendered tree is deterministic.
func sortTree(nodes []*TreeNode) {
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	for _, n := range nodes {
		sortTree(n.Children)
	}
}

// FormatLineage renders the lineage roots as an indented ASCII tree. Each line
// is "<branch><name>  <kind>  <phase>  <node>", with branch glyphs in the style
// of `kubectl tree` / `tree(1)`. Missing phase or node render as a dash. An
// empty set returns a "No sandboxes found" message so an empty cluster reads the
// same as ls.
func FormatLineage(roots []*TreeNode) string {
	if len(roots) == 0 {
		return "No sandboxes found.\n"
	}
	var b strings.Builder
	for i, r := range roots {
		writeTreeNode(&b, r, "", i == len(roots)-1, true)
	}
	return b.String()
}

// writeTreeNode renders one node and recurses into its children. prefix is the
// accumulated indentation for descendants, last reports whether this node is the
// final sibling (so the branch glyph is the corner), and root suppresses the
// glyph for top-level lineage roots.
func writeTreeNode(b *strings.Builder, n *TreeNode, prefix string, last, root bool) {
	branch := ""
	childPrefix := prefix
	if !root {
		if last {
			branch = prefix + "`-- "
			childPrefix = prefix + "    "
		} else {
			branch = prefix + "|-- "
			childPrefix = prefix + "|   "
		}
	}
	b.WriteString(branch)
	b.WriteString(n.Name)
	b.WriteString("  ")
	b.WriteString(n.Kind)
	b.WriteString("  ")
	b.WriteString(orDash(n.Phase))
	b.WriteString("  ")
	b.WriteString(orDash(n.Node))
	b.WriteByte('\n')
	for i, c := range n.Children {
		writeTreeNode(b, c, childPrefix, i == len(n.Children)-1, false)
	}
}
