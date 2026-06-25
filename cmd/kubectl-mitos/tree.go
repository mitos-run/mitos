package main

import (
	"context"
	"fmt"
	"time"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/cli/sandboxtable"
)

// runTree lists all sandboxes in scope and renders their parent->child lineage
// DAG. A non-empty pool scopes to sandboxes sourced from that pool (and the
// forks descended from them); an empty pool renders every lineage in the
// namespace scope.
func runTree(namespace string, allNamespaces bool, pool string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var sandboxes v1.SandboxList
	if err := c.List(ctx, &sandboxes, listOpts(namespace, allNamespaces)...); err != nil {
		return fmt.Errorf("list sandboxes: %w", err)
	}

	items := sandboxes.Items
	if pool != "" {
		items = scopeToPool(items, pool)
	}

	roots := sandboxtable.BuildLineage(items)
	fmt.Print(sandboxtable.FormatLineage(roots))
	return nil
}

// scopeToPool narrows sandboxes to those sourced from the named pool, then
// keeps only the forks reachable from those sandboxes through the
// source.fromSandbox chain (a fork of a fork of a pool sandbox stays in
// scope). It is a transitive-closure walk over the fork source refs.
func scopeToPool(sandboxes []v1.Sandbox, pool string) []v1.Sandbox {
	inScope := make(map[string]bool)
	var kept []v1.Sandbox
	for i := range sandboxes {
		s := &sandboxes[i]
		if s.Spec.Source.PoolRef != nil && s.Spec.Source.PoolRef.Name == pool {
			kept = append(kept, *s)
			inScope[s.Name] = true
		}
	}

	// Iterate to a fixed point: a fork enters scope when its source is in scope.
	// Repeats until no new fork is added, so multi-level chains are covered.
	added := true
	taken := make(map[string]bool)
	for added {
		added = false
		for i := range sandboxes {
			s := &sandboxes[i]
			if taken[s.Name] || s.Spec.Source.FromSandbox == nil {
				continue
			}
			if inScope[s.Spec.Source.FromSandbox.Name] {
				kept = append(kept, *s)
				inScope[s.Name] = true
				taken[s.Name] = true
				added = true
			}
		}
	}
	return kept
}
