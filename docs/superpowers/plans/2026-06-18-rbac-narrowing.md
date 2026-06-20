# Controller Secrets RBAC narrowing (issue #192, path-to-1.0 G3)

**Goal:** the controller must read and write Secrets ONLY in its own namespace
plus the pool namespaces it has adopted, never cluster-wide. Today it holds a
cluster-wide Secrets grant (`get,list,watch,create,update,delete` on all
Secrets in all namespaces), which contradicts the multi-tenant trust boundary
the 1.0 docs claim: a stolen controller ServiceAccount token can read every
Secret in the cluster.

**Status:** shipped behind `controller.namespacedSecretsRBAC` (default false for
safe rollout). Code, chart, threat-model row, and envtest landed in
`feat(controller): narrow controller Secrets RBAC to adopted pool namespaces`.

---

## Why the controller needs Secrets at all

Every Secrets call the controller makes is namespaced in practice; only the
GRANT was cluster-wide. The calls:

- Per-claim mounted Secret reads and the per-sandbox token Secret
  (`sandboxclaim_controller.go`): in the claim's namespace.
- Control plane PKI Secrets (`EnsurePKI`: `mitos-ca`, `mitos-forkd-tls`,
  `mitos-controller-tls`): in the controller's OWN namespace.
- Crypto-shred a template's at-rest encryption key Secret on teardown
  (`DeleteEncKey`): in the template's namespace.
- Replicate husk PKI Secrets (`ReplicateHuskSecrets`: `mitos-ca` ca.crt only and
  `mitos-forkd-tls`, never the CA private key) FROM the controller namespace
  INTO each pool namespace where husk pods run.

So the set of namespaces the controller legitimately touches is exactly
`{controller namespace} ∪ {adopted pool namespaces}`. That is the target scope.

## The escalation knot

Kubernetes forbids privilege escalation through RBAC: a subject cannot create a
Role/RoleBinding/ClusterRole that grants permissions it does not already hold,
unless it either already holds those permissions (cluster-wide, defeating the
purpose here) OR holds the `escalate`/`bind` verb on the role being referenced.

Concretely: if the controller tried to create, per pool namespace, a Role that
itself enumerates `secrets: [get,list,...]` and bind itself to it, the apiserver
rejects the create with a privilege-escalation error UNLESS the controller
already has cluster-wide Secrets (the thing we are removing) or holds `escalate`
on Secrets (a strictly broader and more dangerous grant). Either way we are back
where we started.

## Resolution

Split the grant into a fixed DEFINITION and a per-namespace BINDING, and use the
narrow, resourceName-pinned `bind` verb to let the controller attach the fixed
definition without holding its verbs:

1. **Pre-provisioned `mitos-pool-secrets` ClusterRole (chart-shipped).** It
   enumerates exactly the Secrets verbs the controller needs in a pool
   namespace. It is a DEFINITION only: it is NEVER bound cluster-wide (no
   ClusterRoleBinding references it). An unbound ClusterRole grants nothing.

2. **`bind` verb pinned by `resourceNames` to that one ClusterRole.** The
   controller's own ClusterRole grants
   `rbac.authorization.k8s.io/clusterroles: [bind]` with
   `resourceNames: [mitos-pool-secrets]`. This is the sanctioned
   escalation-prevention bypass: the holder may create a RoleBinding/
   ClusterRoleBinding that references mitos-pool-secrets WITHOUT itself holding
   the Secrets verbs, and may reference NO OTHER role. It is not a general
   escalation primitive: the controller cannot bind any role but this fixed one,
   and cannot edit the ClusterRole's rules (no `update`/`patch` on it).

3. **`rolebindings` create/get/list/watch/delete (+update).** The controller
   manages the per-pool RoleBinding objects themselves. RoleBinding `roleRef` is
   immutable, so the reconcile is create-if-absent, leave-if-present.

4. **Per-pool RoleBinding the controller creates** (`EnsurePoolSecretsRoleBinding`,
   `internal/controller/pool_secrets_rbac.go`). When the controller adopts a
   pool in namespace N (driven from the husk-pod reconcile, next to the existing
   husk TLS/replication setup in `huskpod.go`), it ensures a RoleBinding named
   `mitos-pool-secrets` in N with `roleRef -> ClusterRole/mitos-pool-secrets`
   and a single subject: the controller ServiceAccount
   (`mitos-controller`, in the controller namespace). The controller's effective
   Secrets access in N is then scoped to N, granted BY the binding, not by a
   cluster-wide rule.

5. **Controller's own namespace.** Its PKI and per-claim Secrets there are
   granted by a chart-shipped RoleBinding in the controller namespace (known at
   install time), also referencing mitos-pool-secrets.
   `EnsurePoolSecretsRoleBinding` is a noop when `poolNamespace ==
   controllerNamespace`, so the runtime path never duplicates the chart binding.

6. **Drop the cluster-wide grant.** With `controller.namespacedSecretsRBAC=true`
   the chart REMOVES the cluster-wide `secrets` rule from the controller
   ClusterRole. The controller keeps the `bind` + `rolebindings` grants either
   way. A stolen token then reaches Secrets only in the controller namespace and
   adopted pool namespaces.

```
chart (install time)                       controller (runtime, per adopted pool N)
--------------------                       ----------------------------------------
ClusterRole mitos-pool-secrets  ---------> RoleBinding mitos-pool-secrets in N
  (secrets verbs, NEVER bound                roleRef:  ClusterRole/mitos-pool-secrets
   cluster-wide)                             subject:  SA mitos-controller (ctrl ns)
ClusterRole mitos-controller
  bind on clusterroles
    resourceNames: [mitos-pool-secrets]    (this is what authorizes the create above
  rolebindings: create/get/list/...         without the controller holding secrets)
RoleBinding (ctrl ns) -> mitos-pool-secrets
  subject: SA mitos-controller
```

## Owner-references and GC

The per-pool RoleBinding is additive infrastructure for the controller's own
access, not a child of any single SandboxPool: multiple pools can share a
namespace, and the binding must survive as long as ANY pool in N exists. So it
is intentionally NOT owner-referenced to a single pool (an owner-ref would let
deleting one pool revoke the controller's Secrets access while sibling pools in
N still need it). It is named deterministically (`mitos-pool-secrets`, one per
namespace) and is idempotent to re-ensure. Cleanup when the last pool in a
namespace goes away is left to namespace teardown; a stale binding grants the
controller access only to a namespace it no longer serves, which is benign
(it cannot be used to reach any OTHER namespace). The controller holds
`rolebindings: delete` so a future reaper can remove it, but eager deletion is
not wired up to avoid the multi-pool revocation hazard above.

## Safe staged rollout

The per-pool bindings are created UNCONDITIONALLY (additive) while the
cluster-wide grant is still present (the default). `EnsurePoolSecretsRoleBinding`
failures are NON-FATAL in the reconcile (logged at V(1), husk pods keep being
created), because the binding is load-bearing only once the cluster-wide grant
is removed, and a partial RBAC mirror or rollout ordering gap must not break the
warm pool. Operators:

1. Upgrade with `namespacedSecretsRBAC=false` (default). The chart ships the
   ClusterRole, the controller's own-namespace binding, and the bind/rolebindings
   grants; the controller starts creating per-pool bindings.
2. Let the controller reconcile every live pool namespace (bindings appear).
3. Flip `namespacedSecretsRBAC=true`. The chart removes the cluster-wide Secrets
   rule. Access never drops mid-flight because the bindings were already in
   place before the grant was removed.

## Deliverables (all landed)

- `internal/controller/pool_secrets_rbac.go`: `EnsurePoolSecretsRoleBinding`,
  the constants `ControllerServiceAccountName`, `PoolSecretsClusterRoleName`,
  `PoolSecretsRoleBindingName`.
- Call site in `internal/controller/huskpod.go` (per adopted pool, non-fatal).
- `deploy/charts/mitos/templates/pool-secrets-rbac.yaml`: the ClusterRole
  definition plus the controller-namespace RoleBinding (gated on the toggle).
- `deploy/charts/mitos/templates/clusterrole.yaml`: cluster-wide Secrets rule
  gated OFF when `namespacedSecretsRBAC=true`; `bind` (resourceName-pinned) +
  `rolebindings` grants always present.
- `deploy/charts/mitos/values.yaml`: `controller.namespacedSecretsRBAC` (false),
  `controller.poolSecretsClusterRoleName` (mitos-pool-secrets).
- `config/rbac/role.yaml` and `deploy/rbac/clusterrole.yaml`: surfaced copies
  carry the bind + rolebindings grants (the raw cluster-wide Secrets rule stays
  in the surfaced ClusterRole because that un-templated manifest is the
  pre-narrowing default; the narrowing is exercised via the chart toggle).
- `docs/threat-model.md`: the "Controller RBAC for Secrets" row updated to
  "narrowing shipped, opt-in" with the residuals recorded (the controller can
  still write Secrets into any adopted pool namespace; the cluster-wide `pods`
  create/delete grant for husk pods is unchanged).
- Envtest: `TestEnsurePoolSecretsRoleBindingCreatesNamespacedBinding` (binding
  references mitos-pool-secrets, binds the controller SA, idempotent) and
  `TestEnsurePoolSecretsRoleBindingSameNamespaceIsNoop`.

## Residuals (tracked, out of scope for #192)

- The controller can still write Secrets into ANY adopted pool namespace (it
  must, for token + PKI replication). Per-pool write scoping with a tighter
  Role is a later hardening step.
- The cluster-wide `pods` create/delete grant (husk warm pool) is unchanged.
- Eager RoleBinding GC on last-pool-in-namespace removal is deferred (the
  multi-pool shared-namespace revocation hazard above).
