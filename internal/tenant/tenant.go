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

// namespacePrefix is the prefix for an org's hard-isolation namespace.
const namespacePrefix = "mitos-org-"

// NamespaceForOrg returns the namespace an org's workloads live in under hard
// isolation. The same mapping backs the kube secret provider and the
// org-scoped sandbox query, so secrets and sandboxes share one tenant boundary.
func NamespaceForOrg(orgID string) string {
	return namespacePrefix + orgID
}

// OrgLabels returns the standard label set stamping an object as owned by orgID.
func OrgLabels(orgID string) map[string]string {
	return map[string]string{OrgLabelKey: orgID}
}
