// Package tenant holds the canonical multi-tenancy convention shared across the
// SaaS surfaces: the org label stamped on tenant-owned objects and the hard
// per-org namespace each tenant's workloads live in.
//
// Hard isolation (the chosen model): every org gets its own namespace,
// mitos-org-<id>, for both its sandboxes and its secrets. The org label lets the
// console and the usage pipeline resolve and filter by org regardless of where
// an object lives, and is the contract the gateway/control plane stamps at
// SandboxClaim creation (the controller propagates it onto the Sandbox).
package tenant

// OrgLabelKey is the label carrying an org id on tenant-owned objects
// (SandboxClaim, Sandbox, org Secrets). It is the contract the gateway stamps
// and the console / usage resolver read; changing it is a breaking change.
const OrgLabelKey = "mitos.run/org"

// RegionLabelKey is the label carrying a placement value (issue #712 phase
// 0's placement.Registry) on a tenant-owned Sandbox: the console's cluster
// adapter stamps it on every tree root at creation time (empty region means
// the deployment's registry default, so nothing is stamped at all), and a
// fork MUST copy it verbatim from its parent rather than re-resolving it,
// because a live CoW fork cannot cross clusters (region is a property of the
// fork tree, not of each fork individually). The usage pipeline reads it
// best-effort at attribution time; an object with no label (created before
// this existed, or on a deployment that never set one) simply has an empty
// region.
const RegionLabelKey = "mitos.run/region"

// namespacePrefix is the prefix for an org's hard-isolation namespace.
const namespacePrefix = "mitos-org-"

// NamespaceForOrg returns the namespace an org's workloads live in under hard
// isolation. The same mapping backs the kube secret provider and the
// org-scoped sandbox query, so secrets and sandboxes share one tenant boundary.
func NamespaceForOrg(orgID string) string {
	return namespacePrefix + orgID
}

// OrgFromNamespace is the exact inverse of NamespaceForOrg: it recovers the org
// id from a hard-isolation namespace mitos-org-<id>, returning ok=false for any
// namespace that is not an org namespace (default, mitos, kube-system, a
// self-host single-tenant namespace).
//
// This is the TRUSTED billing attribution source. The control plane places a
// tenant's workloads in mitos-org-<id>, so the namespace is a control-plane fact,
// not client input. The controller derives the org from the namespace via this
// helper and stamps OrgLabelKey; a client-set org label is never trusted. A
// non-org namespace returns ("", false) so self-host stays unattributed rather
// than being forced into a bogus org.
func OrgFromNamespace(ns string) (orgID string, ok bool) {
	if len(ns) <= len(namespacePrefix) {
		return "", false
	}
	if ns[:len(namespacePrefix)] != namespacePrefix {
		return "", false
	}
	return ns[len(namespacePrefix):], true
}

// OrgLabels returns the standard label set stamping an object as owned by orgID.
func OrgLabels(orgID string) map[string]string {
	return map[string]string{OrgLabelKey: orgID}
}
