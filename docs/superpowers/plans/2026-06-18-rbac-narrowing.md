# Controller Secrets RBAC narrowing (G3, #192)

> **Status: design resolved, implementation shipped.** This is the design record for
> deliverable 1 of #192. It documents the escalation knot and the resolution that the code,
> chart, and threat model already implement; the pointers at the end are the shipped surface.

**Goal.** The controller must reach Secrets ONLY in its own namespace plus the pool
namespaces it has adopted, never cluster-wide. A cluster-wide Secrets grant is incompatible
with the multi-tenant trust boundary 1.0 claims (G3): a stolen controller token would read
every Secret in the cluster, not just the ones the controller legitimately handles (its PKI
and per-claim Secrets in the controller namespace, and the replicated husk PKI plus
per-sandbox token Secrets in pool namespaces).

## The escalation knot

Kubernetes will not let a subject grant permissions it does not itself hold. To create a
RoleBinding that confers the Secrets verbs, the creator must EITHER already hold those verbs
(here, cluster-wide, which is exactly what we are removing) OR hold the `bind` verb on the
referenced (Cluster)Role (the `rbac.authorization.k8s.io` escalation-prevention rule). So the
controller cannot simply self-provision namespaced Secrets access at runtime: the naive
"create a Role and bind it per namespace" needs a privilege the narrowed controller is not
supposed to have.

## Resolution

Split the Secrets authority into a definition the chart ships once and a binding the
controller creates per pool namespace, and unlock the binding with a tightly scoped `bind`
grant rather than the Secrets verbs themselves:

1. **A `mitos-pool-secrets` ClusterRole, definition only.** The chart ships a ClusterRole
   holding the Secrets verbs the controller needs (get, list, watch, create, update, delete).
   It is NEVER bound cluster-wide. A ClusterRole that is never the target of a
   ClusterRoleBinding grants nothing on its own; it is just a reusable rule set.

2. **A `bind` grant scoped by `resourceNames` to that one ClusterRole.** The controller's own
   ClusterRole grants `bind` on `clusterroles` with `resourceNames: [mitos-pool-secrets]`,
   plus `create` on `rolebindings`. This is the sanctioned escalation-prevention bypass: with
   `bind` on a specific role, a subject may create RoleBindings referencing exactly that role
   without holding its verbs. Because it is pinned to the single `mitos-pool-secrets`
   ClusterRole, it is not a general escalation primitive: the controller cannot bind any other
   role, so it cannot grant itself (or anyone) arbitrary permissions.

3. **A RoleBinding per pool namespace, created by the controller.** As the controller adopts a
   pool namespace, it creates a namespaced RoleBinding there that binds its ServiceAccount to
   `mitos-pool-secrets` (`EnsurePoolSecretsRoleBinding`). The grant is namespaced, so it
   confers Secrets access only in that namespace. The function is additive and idempotent: a
   present binding is left as is because a RoleBinding's `roleRef` is immutable, and binding
   into the controller's own namespace is a noop (the chart ships that binding directly, since
   the controller namespace is known at install).

4. **Drop the cluster-wide grant behind a rollout flag.** With `controller.namespacedSecretsRBAC=true`
   the chart removes the cluster-wide Secrets grant, leaving the controller with Secrets access
   only in its own namespace and adopted pool namespaces. It defaults to false for a safe
   rollout: the per-pool RoleBindings must reconcile across all live pool namespaces FIRST
   (the controller creates them while it still holds the cluster-wide grant), and only then
   does the operator flip the value to drop the grant. Flipping early would break Secrets
   access in any pool namespace whose binding had not yet reconciled.

## Why this is the right shape

- The `bind`-scoped-by-`resourceNames` grant is the minimum privilege that resolves the knot.
  The alternative (holding the Secrets verbs cluster-wide to create the bindings) is the
  status quo we are removing.
- The ClusterRole-as-definition plus per-namespace RoleBinding keeps the rule set in one place
  while making every actual grant namespaced and visible (`kubectl get rolebinding -A`).
- The rollout flag makes the change safe to apply to a running cluster with live pools, rather
  than a flag day that risks a window where the controller cannot read tenant Secrets.

## Residual risk

- The controller can still write Secrets into any pool namespace it adopts. This is inherent:
  it replicates husk PKI and mints per-sandbox token Secrets there. The trust boundary is "the
  controller is trusted within the namespaces it manages," not "the controller cannot touch
  tenant Secrets."
- The cluster-wide `pods` create/delete grant (husk pods run in pool namespaces) is unchanged;
  this slice narrows Secrets only.
- A live cluster left on the default (`namespacedSecretsRBAC=false`) still holds the
  cluster-wide grant. The narrowing is real only once an operator completes the rollout.

## Shipped surface (the rest of #192)

- Code: `internal/controller/pool_secrets_rbac.go` (`EnsurePoolSecretsRoleBinding`), called
  from the pool reconcile path in `internal/controller/huskpod.go` as a namespace is adopted.
- Chart: `deploy/charts/mitos/templates/pool-secrets-rbac.yaml` (the ClusterRole definition,
  the own-namespace RoleBinding, and the `namespacedSecretsRBAC` gate) and the `bind`/
  `rolebindings` grant in the controller ClusterRole (`clusterrole.yaml`, mirrored in
  `config/rbac/role.yaml`).
- Tests: `internal/controller/pool_secrets_rbac_test.go` (envtest) proves the namespaced
  binding is created against a real apiserver, that a re-run is idempotent, and that binding
  into the controller's own namespace is a noop.
- Threat model: the "Controller RBAC for Secrets" row in `docs/threat-model.md` records the
  narrowing, the scoped `bind` grant, the rollout default, and the residual risks above.

## Sequencing

This is part of G3 (multi-tenant isolation) and is sequenced before G4 (external security
review, #194) so the reviewed surface is the narrowed one.
