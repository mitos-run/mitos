# Day-2 operations: upgrade, rollback, backup, uninstall

This is the operator lifecycle guide for a self-hosted Mitos install deployed
with the Helm chart (`deploy/charts/mitos`). It assumes the install path from
the chart README and `prerequisites.md` and covers everything after that first
install: upgrading, the CRD step Helm does not do for you, rolling back, backing
up the state that matters, and uninstalling cleanly.

Versioning: the chart `version` and `appVersion` are bumped together by release
automation, so a chart release normally ships the matching component images.
Component release tags are `vX.Y.Z`; the chart release is tagged
`mitos-<chart-version>` by the chart releaser. Read the release notes at
https://github.com/mitos-run/mitos/releases before any upgrade.

## Upgrading

The order is: read the release notes, apply the CRDs, then `helm upgrade`.

1. Check the release notes for the target version, and for every version you
   skip over, for called-out breaking changes.

2. Apply the CRDs for the target version (see CRD upgrades below). Helm will
   not do this; upgrading the controller against an older CRD schema is not a
   supported state.

3. Upgrade the release. Pass the same values files you installed with; avoid
   `--reuse-values`, which suppresses new chart defaults introduced by the
   upgrade.

   From the HTTP Helm repository:

   ```bash
   helm repo update
   helm upgrade mitos mitos/mitos -n mitos \
     --set namespace.create=false -f your-values.yaml
   ```

   From the OCI registry:

   ```bash
   helm upgrade mitos oci://ghcr.io/mitos-run/charts/mitos -n mitos \
     --version <chart-version> \
     --set namespace.create=false -f your-values.yaml
   ```

   From a checkout of the matching release tag:

   ```bash
   helm upgrade mitos deploy/charts/mitos -n mitos \
     --set namespace.create=false -f your-values.yaml
   ```

4. Watch the rollout:

   ```bash
   kubectl -n mitos rollout status deploy/mitos-controller
   kubectl -n mitos rollout status ds/mitos-forkd
   kubectl -n mitos get pods
   ```

### What happens to running sandboxes during the roll

- **Controller** (Deployment, 2 replicas with leader election): running
  sandboxes are untouched. The controller holds no in-memory desired state; on
  restart it rebuilds everything from the CRDs and reconciles within one GC
  interval (see `docs/failure-gc.md`, "Controller-restart reconciliation").
  During the leader hand-off new claims and reconciles pause briefly, then
  resume.
- **forkd** (DaemonSet, `RollingUpdate` with `maxUnavailable: 1`): one node
  rolls at a time. In the default husk-pods mode, running sandbox VMs live
  inside per-sandbox husk pods, not the forkd pod, and exec/file traffic goes
  to the husk pod's own endpoint, so running sandboxes keep serving through a
  forkd roll. What pauses on the rolling node until the new forkd pod is Ready:
  template snapshot builds, peer snapshot distribution, and new raw-mode forks.
  A restarted forkd re-adopts still-live VMs it launched directly from its
  on-disk journal (see `docs/failure-gc.md`).
- **Raw-forkd mode** (`--enable-raw-forkd`, the fallback path): the Firecracker
  processes are children of the forkd pod, so replacing that pod for an image
  upgrade tears them down with it. What happens to those Sandbox objects
  afterwards is not covered by a documented guarantee; treat the survival of
  running raw-mode sandboxes across a forkd upgrade as undefined and plan the
  roll for a window where losing them is acceptable.
- **Console** (1 replica by default): a brief downtime while the pod replaces.
  Browser sessions are held in pod memory, so users are signed out and sign in
  again. Accounts, orgs, and API keys live in the Postgres database (when
  `database.dsnSecretRef` is set) and are unaffected.
- **Gateway** (2 replicas, rolling): API-key requests keep flowing during the
  roll. The abuse/billing suspension (kill-switch) store is in-process and does
  not survive a restart; re-drive any active suspensions after the upgrade.
- **Warm pools and husk pods** are controller-managed objects, not chart
  resources; a `helm upgrade` does not restart them.

## CRD upgrades

Helm treats the chart's `crds/` directory specially, and it is worth being
precise about what that means: the CRDs are installed once, on the first
`helm install`, and after that Helm never touches them again. `helm upgrade`
does not apply schema changes to them, `helm rollback` does not revert them,
and `helm uninstall` does not delete them. This is deliberate upstream Helm
behavior, because deleting or mishandling a CRD destroys every custom resource
of that kind cluster-wide. The consequence for you as the operator: every chart
upgrade that ships a CRD schema change requires you to apply the new CRD
manifests yourself, before the `helm upgrade`.

The chart ships five CRDs, all in API group `mitos.run/v1`: `sandboxpools`,
`sandboxes`, `workspaces`, `workspacerevisions`, and `orgs`. The versioned
manifests live in the repo at `deploy/charts/mitos/crds/` and inside the
published chart archive under `mitos/crds/`.

From a checkout of the target release tag:

```bash
git fetch --tags && git checkout v<version>
kubectl apply --server-side --force-conflicts -f deploy/charts/mitos/crds/
```

Or from the published chart, without a checkout:

```bash
helm pull oci://ghcr.io/mitos-run/charts/mitos \
  --version <chart-version> --untar --untardir /tmp/mitos-chart
kubectl apply --server-side --force-conflicts -f /tmp/mitos-chart/mitos/crds/
```

Server-side apply avoids the client-side `last-applied-configuration`
annotation and its size limit on large CRD manifests; `--force-conflicts` is
needed because the fields are currently owned by the manager that created the
CRDs (Helm on first install, or a previous `kubectl apply`). Applying a CRD
never touches the stored custom resources; it only updates the schema they are
validated against.

Verify:

```bash
kubectl get crds | grep mitos.run
```

## Rolling back

```bash
helm history mitos -n mitos
helm rollback mitos <revision> -n mitos
```

`helm rollback` re-applies the manifests of the target revision as a new
revision. It rolls back everything the chart templates: the controller, forkd,
device plugin, kernel provisioner, facade, console, and gateway images and
their configuration. Those components are stateless or rebuild their state, so
rolling them back is safe in itself. Two things do NOT roll back with them:

- **CRDs.** A rollback never downgrades CRDs; the newer schema stays in place
  (the same Helm `crds/` behavior as above). An older controller reading
  objects with newer optional fields ignores what it does not know, but when it
  writes an object back it can drop values in fields it never knew about. If
  the release notes for the version you are leaving call out a breaking CRD
  change, do not `helm rollback` across it; restore cluster state from backup
  instead.
- **The Postgres schema.** The console and gateway apply embedded, forward-only
  schema migrations at startup, recorded in a `schema_migrations` table. There
  is no down-migration path, and running an older binary against a schema a
  newer version migrated is untested; treat it as undefined. The reliable
  rollback for the database is restoring the pre-upgrade dump (next section),
  which is why you take one before upgrading.

Node-local template snapshots and CAS content are keyed by content, not by
chart revision, and are unaffected by a rollback.

## Backing up state

What state lives where:

| State | Lives in | Backup approach |
| --- | --- | --- |
| Accounts, orgs, memberships, API keys, sessions, credit ledger, usage records, spend caps, allowlist | The Postgres database referenced by `database.dsnSecretRef` | `pg_dump` (below); managed-Postgres PITR in production |
| Pool, workspace, revision, and org objects (the declarative config and revision DAG metadata) | CRs in the cluster (etcd) | Cluster backup (etcd snapshots, Velero) or `kubectl get ... -o yaml` exports; GitOps for the specs |
| Running sandbox state | microVMs on the KVM nodes | Not backupable; sandboxes are ephemeral by design. A re-created Sandbox object does not restore a VM |
| Template snapshots, CAS chunks, fork CoW state | `forkd.dataDir` (default `/var/lib/mitos`) on each KVM node | None needed: pools rebuild template snapshots from their spec, and snapshot distribution re-fans them out (see `docs/failure-gc.md`) |
| Workspace revision content | The node CAS under `forkd.dataDir` by default, or an S3-compatible bucket when the Workspace spec configures the S3 store | Node-CAS-backed revisions are node-local data and are lost with the node disk; use the Workspace S3 store for durable workspace content and back the bucket up per your object-store practice |
| Operator-created Secrets (database DSN, OIDC client secrets, expose secrets, SMTP and billing credentials, telemetry salt) | Kubernetes Secrets in the install namespace | Keep the sources of truth in your secret manager or sealed-secret GitOps flow; they are not derivable from anything else |
| Control plane PKI (`mitos-ca`, `mitos-forkd-tls`, `mitos-controller-tls`) | Kubernetes Secrets in the install namespace | Self-healing: the controller mints missing PKI Secrets at startup, so no backup is required |

The one store you must treat as production data is the Postgres database: it
holds every account, organization, API key, and the credit ledger. The chart is
bring-your-own Postgres by design, and the recommended production path is a
managed database (RDS, Cloud SQL, Neon, and so on) with point-in-time recovery
enabled; PITR restores to any moment and needs no dump schedule. A `pg_dump`
taken before every upgrade is the floor, not the ceiling.

Dump, using the same DSN the cluster uses (the example assumes the Secret name
from the chart README, `mitos-db`, key `dsn`):

```bash
DSN=$(kubectl -n mitos get secret mitos-db -o jsonpath='{.data.dsn}' | base64 -d)
pg_dump --format=custom --no-owner --file="mitos-$(date +%Y%m%d).dump" "$DSN"
```

Restore. Scale the writers down first so nothing writes mid-restore, then scale
back up (the console and gateway re-apply their migrations idempotently on
startup):

```bash
kubectl -n mitos scale deploy/mitos-console deploy/mitos-gateway --replicas=0
pg_restore --clean --if-exists --no-owner --dbname="$DSN" mitos-<date>.dump
kubectl -n mitos scale deploy/mitos-console --replicas=1
kubectl -n mitos scale deploy/mitos-gateway --replicas=2
```

If `database.dsnSecretRef` is unset, the console and gateway run on in-memory
storage: there is nothing to back up and everything is lost on every pod
restart. That mode is for development only.

## Uninstalling

Delete the workload objects FIRST, while the controller and forkd are still
running: the Sandbox finalizer reap needs them. Uninstalling the chart first
leaves Sandbox objects wedged in Terminating with a finalizer no controller
will ever remove.

```bash
kubectl delete sandboxes --all --all-namespaces
kubectl delete sandboxpools --all --all-namespaces
kubectl get sandboxes --all-namespaces   # wait until empty
helm uninstall mitos -n mitos
```

What intentionally survives `helm uninstall`:

- **The CRDs and any remaining CRs** (Workspaces, WorkspaceRevisions, Orgs, and
  anything you chose not to delete above). Helm never deletes `crds/` content,
  by design.
- **The install namespace**, when you created it yourself per the documented
  install path (`namespace.create=false`). Out-of-band Secrets in it (the
  database DSN, OIDC, expose, billing Secrets, and the controller-minted PKI)
  go away only with the namespace.
- **Node-local data** under `forkd.dataDir` (default `/var/lib/mitos`) on each
  KVM node: it is a hostPath, so nothing in the cluster deletes it.
- **The Postgres database**, which is external by design.
- PVCs: the chart creates none, so there are none to clean up.

Full cleanup, after the uninstall:

```bash
# WARNING: deleting a CRD deletes EVERY object of that kind cluster-wide,
# including workspace revision history. There is no undo.
kubectl delete crd sandboxes.mitos.run sandboxpools.mitos.run \
  workspaces.mitos.run workspacerevisions.mitos.run orgs.mitos.run

kubectl delete namespace mitos
```

Then remove the data dir on each KVM node out of band (for example
`rm -rf /var/lib/mitos` over SSH, or the equivalent on Talos), and drop or
retire the Postgres database per your data-retention policy.

If a Sandbox is already wedged in Terminating because the controller was
uninstalled first, remove the finalizer by hand and let the GC-less delete
finish, accepting that the backing VM (if any survives) is reaped only by the
node teardown:

```bash
kubectl patch sandbox <name> -n <namespace> --type=merge \
  -p '{"metadata":{"finalizers":[]}}'
```
