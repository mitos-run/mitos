# Pre-claimed checkout: the gateway hands out an already-activated sandbox

Status: approved direction (2026-07-10, jannes: gateway buffer, k8s-native labels,
remaining decisions delegated to the recommended options). Companion to the
prepare-time restore plan (docs/superpowers/plans/2026-07-10-prepare-time-restore.md),
which attacks the engine share of the same budget.

## Why

Hosted TTI is ~308 ms P50 (bench/results/2026-07-10-tti-hosted.md). The measured
create budget (prod controller logs, 2026-07-10, successful claims n=33) is:
controller reconcile 137 ms P50 (activate_rpc 98.2, mark_pod_claimed 14.8,
status_write_ready 8.7) plus ~35 ms of k8s plumbing around it plus ~20 ms gateway
front. Even with prepare-time restore slices 2 and 3 landed (activate ~30 ms,
first exec warm ~35 ms), the projected TTI is ~165 ms: above Daytona (136 ms) and
far above Northflank (95.9 ms) on the ComputeSDK peer table.

The remaining cost is the claim round trip itself: CR write, watch wake-ups,
reconcile, activate RPC, status writes. The peers we trail do not pay any of this
per request; their sequential number is a warm-pool checkout (their burst ratios
prove it, 5.2x for Daytona). This design gives mitos the same checkout shape:
create() pops a sandbox that is ALREADY Ready, and the only hot-path write is the
attribution patch.

## Decision record

1. **Placement: gateway buffer of real Sandboxes** (chosen over husk-level
   generic-activate + attach). Fastest to the target, no controller, husk, or
   guest changes, no overlap with the prepare-time restore workstream. The trade
   is a dependency on the single-tenant namespace (see Migration).
2. **Source of truth: k8s labels, not gateway memory or Postgres.** Buffered
   sandboxes carry `mitos.run/buffered: "true"`. Checkout is one
   resourceVersion-guarded label patch that atomically removes the buffered label
   and stamps the org labels: mutual exclusion between the two gateway replicas
   AND claim-time org attribution in the same ~10 ms write. Memory is only a
   cache; a restarted gateway re-adopts by LIST.
3. **Eligibility: pool-only creates.** A create qualifies iff it names a
   checkout-enabled pool and sets no env, no secrets, no workspace, no
   replicas > 1, and no TTL or timeout. Everything else, and every buffer miss,
   takes the classic path unchanged. Rationale: env and secret values are
   delivered by the fork-correctness handshake at activation, which for a
   buffered sandbox already happened with empty tenant inputs.
4. **Token: the pre-minted token is reused, not rotated.** The gateway already
   transits every sandbox token; the token lives in the controller-owned Secret
   (which the gateway already reads) and in gateway memory. Rotation would cost
   a husk round trip and re-serialize the path this design removes.
5. **Buffer sizing: floor 2, cap 4, per-pool opt-in, maxAge 10 m.** Hosted prod
   enables it for the `python` pool only. Refill is triggered by a pop and by a
   15 s reconcile; both replicas count before creating (LIST), overshoot is
   bounded by the cap. Each buffered sandbox holds one warm-pool slot and ~72 MiB
   resident (lazy-restore measurement), platform-paid; floor 2 keeps that cost
   under one extra dormant pod equivalent.
6. **Billing and quota: attribution at claim.** A buffered sandbox has NO org
   labels, so the usage scraper (trusted pod label) bills nobody and the console
   shows it to nobody; that unbilled window is deliberate platform cost. At
   checkout the CR patch stamps the org synchronously (runtime authz getOwned
   needs it before the first exec arrives); the husk POD org label (billing) is
   patched asynchronously with retry, and the janitor sweeps for org-labeled CRs
   whose pod is still org-less so no claimed sandbox can dodge metering for more
   than one sweep interval. Quota is checked before checkout, exactly where the
   classic path checks it before the CR create.
7. **Reaping: the buffered label exempts an entry from idle reap; the janitor
   terminates buffered sandboxes older than maxAge.** A buffered sandbox is idle
   by definition, so without the exemption the idle reaper would drain the
   buffer. maxAge bounds staleness (clock and CRNG were stepped at activation;
   the VM runs live while buffered) and bounds leak size if refill misbehaves.
8. **Health: the reconcile prunes non-Ready buffer entries.** A husk that dies
   while buffered is dropped from the buffer at the next 15 s pass; a pop that
   races a just-died sandbox surfaces to the tenant like any post-create death
   does today. No per-pop health probe (it would re-serialize a round trip).

## Data flow

Refill (off the hot path, per gateway replica):

1. LIST sandboxes with `mitos.run/buffered=true` for the pool; if count is at or
   above floor, stop.
2. Create a Sandbox through the EXISTING create path (watch-before-create, #895)
   with `mitos.run/buffered: "true"`, no org labels, in the shared namespace.
3. On Ready, cache {name, endpoint, token} in memory (token from the create
   response it already receives).

Checkout (the hot path):

1. Gateway front as today: auth, quota, parse. Eligibility check (decision 3).
2. Pop a cached entry; issue ONE patch (resourceVersion-guarded): remove
   `mitos.run/buffered`, set `tenant.OrgLabels(org)`. A 409 means the other
   replica won that entry: drop it, pop the next; empty buffer means classic
   path.
3. Return 201 {id, endpoint, token, phase Ready} from cache. fork_time_ms is the
   observed wall time of this request, honestly tiny.
4. Async: patch the husk pod org label (billing); kick refill.

Adopt (gateway start): LIST buffered sandboxes, read each token Secret, rebuild
the memory cache.

Janitor (each reconcile pass): prune non-Ready entries; terminate entries older
than maxAge; re-patch pods for claimed CRs still missing the pod org label.

## Security

- Before checkout a buffered sandbox has no org labels, so getOwned matches NO
  caller: it is unreachable through the gateway runtime surface. Its endpoint is
  cluster-internal and bearer-token gated exactly like every Ready sandbox.
- No tenant data exists in a buffered VM: eligibility excludes env, secrets, and
  workspaces, and the activation handshake ran with empty tenant inputs.
- The token custody chain is unchanged in kind (controller Secret -> gateway ->
  tenant); what changes is duration (up to maxAge in gateway memory). Never
  logged, as today.
- threat-model.md gets a row for the buffered state in the same PR.

## Failure behavior (operating principle 4)

- Gateway crash: memory cache lost, CRs remain; the surviving replica and the
  restarted one re-adopt by LIST. Nothing leaks past maxAge.
- Both replicas down: buffered sandboxes idle until the janitor of whichever
  replica returns reaps the over-age ones.
- Slow etcd: the checkout patch inherits it (one write); classic path inherits
  it worse (several). Fallback ordering never blocks on the buffer.
- Capacity exhaustion: refill creates fail like any create (NoHuskPod pending);
  the buffer floor is best-effort, the hot path falls back to classic.
- The 1752-retry pathology (#894) applies to refill creates too; refill backs
  off on consecutive failures and never retries in a tight loop.

## Honesty and measurement

- No README or website number changes from this design until
  bench/tti-latency.py reproduces the improvement against prod, per the
  no-unverified-claims rule. The bench doc must state that eligible creates are
  buffer checkouts and report the measured fallback (buffer-miss) cost beside
  the headline, the same honesty bar we apply to Daytona's burst ratio.
- Projection, stated as a target not a claim: create ~35-50 ms server-side
  (front ~20 + patch ~10 + response), TTI ~95-110 ms with prepare-time restore
  slice 3, near the Northflank line. The next visible chunks after this land
  are the per-create quota LIST and the activate RPC overhead (#893 adjacent).

## Migration note (per-org namespaces)

This design leans on the single-tenant namespace: a CR cannot move namespaces,
so per-org tenancy invalidates a shared buffer. At that cutover the options are
per-org buffers (paid per org, only above an activity threshold) or the
husk-level generic-activate + attach design (pre-activation at the POD, claim
stays a per-org CR create). This spec explicitly does NOT choose; the checkout
surface (eligibility gate, fallback ordering, bench honesty) survives either.

## Configuration

Helm: `gateway.checkout.pools` (list, default empty = feature off),
`gateway.checkout.floor` (default 2), `gateway.checkout.cap` (default 4),
`gateway.checkout.maxAge` (default 10m). Self-host gets the same knobs; the
feature is capability-neutral (no console surface change).

## Testing

- Unit (fake client, in-package with the #895 tests): eligibility gate;
  pop-patch exclusivity (two concurrent pops, one 409 loser re-pops); fallback
  on empty buffer; adopt-on-start rebuilds cache from CRs + Secrets; janitor
  prunes non-Ready, reaps over-age, re-patches org-less pods; refill respects
  floor/cap and backs off on failures; org labels land on CR before the
  response returns.
- kind e2e: a checkout-enabled pool serves a create from the buffer (observable
  via the buffered label lifecycle) and a create with env falls back classic.
- Prod acceptance: bench/tti-latency.py before/after, plus one burst run to
  measure the fallback cost honestly.
