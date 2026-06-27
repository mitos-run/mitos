# agents.x-k8s.io facade conformance

This document records how the `agents.x-k8s.io` conformance facade
(`cmd/facade` + `internal/facade`) is held to the upstream SIG agent-sandbox API,
what is PROVEN today, and what is deferred. See ADR 0001
(docs/adr/0001-facade-and-naming.md) for the facade design and the toolchain
decision.

## Principle: no silent divergence

We implement the upstream API (`agents.x-k8s.io/v1beta1 Sandbox`); we do not
fork or shadow it. Every upstream artifact, each `test/e2e/*_test.go` and each
vendored example manifest, has a row in the matrix below with one of four
statuses. There is no fifth status and no omission: an undocumented divergence
is a bug.

- PROVEN-OBJECT-LEVEL-ON-KIND: the facade-conformance CI job (or the facade
  envtest) asserts the object-level fact on a kind cluster, with their manifest
  applied UNCHANGED.
- PROVEN-ON-KVM: the upstream predicate that needs a RUNNING sandbox (a booted
  in-VM workload reaching Ready) is asserted on a real KVM cluster by the
  `cluster-facade-conformance-e2e` job (`.github/workflows/cluster-e2e.yaml`,
  `test/cluster-e2e/facade-conformance-kvm.sh`): the upstream Sandbox applied
  UNCHANGED reaches Ready=True on a real booted Firecracker VMM through the
  facade, exec-through the bridged sandbox is live, and the operatingMode resume
  tail re-activates.
- NEEDS-BARE-METAL: the upstream predicate requires a RUNNING sandbox of a
  SPECIFIC workload shape we do not run on the KVM job (ChromeReady on the CDP
  port; the python-runtime serving check; the upstream Pod/Service objects their
  in-tree controller creates). These remain bare-metal-gated and workload
  specific; the GENERIC in-VM Ready predicate is PROVEN-ON-KVM above.
- JUSTIFIED-EXCEPTION: a field or behavior the facade maps differently (or does
  not yet map), with the reason. The behavior is recorded, not silently dropped.

## Pinned upstream version

- Module: `sigs.k8s.io/agent-sandbox`, version `v0.5.0` (pinned). The CRDs,
  examples, and `test/e2e` are vendored verbatim under `third_party/agent-sandbox/`.
  v0.5.0 graduated the API to `v1beta1` (the storage version; `v1alpha1` is still
  served but deprecated). The facade imports and serves `agents.x-k8s.io/v1beta1`
  and `extensions.agents.x-k8s.io/v1beta1`.
- Conversion webhook accommodation: the vendored CRD ships
  `conversion.strategy: Webhook` whose clientConfig targets upstream's controller
  (`agent-sandbox-system/agent-sandbox-webhook-service`, no caBundle). We do NOT
  run upstream's controller or its conversion webhook, and we exercise conformance
  ONLY at the `v1beta1` stable/storage version. The envtest suite registers only
  `v1beta1`; the kind `facade-conformance` job applies the vendored CRDs unchanged
  and then pins the LIVE CRD's `conversion.strategy` to `None` at install time (the
  vendored file on disk is never edited, so apply-unchanged is preserved). This is
  a test-infra accommodation, not an API divergence: the surface we test is the
  graduated stable version.
- Latest-two-minors policy: the conformance approach tracks the upstream API as
  it moves by pinning their latest two minor releases. Today only v0.5.0 is
  wired (vendored + applied unchanged). Wiring the second minor (a parallel
  vendored tree + a CI matrix dimension) is a follow-up; this is stated honestly
  rather than implied.

## Apply-unchanged acceptance

Their example `Sandbox` manifests apply UNCHANGED (only the `${IMAGE}`
placeholder is substituted, on a copy; the vendored files are never edited) and
the facade reconciles them object-level:

- In envtest (`internal/facade/examples_test.go`): every core
  `agents.x-k8s.io/v1beta1` Sandbox example vendored under
  `third_party/agent-sandbox/examples` (and `extensions/examples`) is applied and
  the facade creates the bridged husk-backed `Sandbox` (default-pool
  binding, owner reference, first-container env mirrored).
- In CI (`facade-conformance` kind job): the upstream
  `examples/hello-world-sandbox/hello-world.yaml` is applied UNCHANGED against a
  live apiserver and the five object-level facts below are asserted.

The facade now maps the core `Sandbox` AND all three
`extensions.agents.x-k8s.io` extension kinds (`SandboxTemplate`,
`SandboxWarmPool`, `SandboxClaim`). Their extension example manifests apply
UNCHANGED too: `internal/facade/extension_reconciler_test.go` maps each in
envtest, and the `facade-conformance` kind job applies `sandboxtemplate.yaml` +
`sandboxwarmpool.yaml` + `sandbox-claim.yaml` unchanged and asserts the
object-level facts (g)-(j) below.

The RUNNING-sandbox behavior (the in-VM Ready tail) is the bare-metal path and is
NOT asserted on kind; see the NEEDS-BARE-METAL rows.

## The facade-conformance CI job (object level)

The `facade-conformance` job in `.github/workflows/ci.yaml` mirrors
`kind-e2e-husk`: it builds + loads the facade and controller images, creates a
kind cluster, installs the vendored upstream `agents.x-k8s.io` Sandbox CRD plus
our CRDs, deploys our controller + the facade (PKI on, `--default-pool=default`)
and a `default` SandboxPool, applies their hello-world Sandbox UNCHANGED, and
asserts, each gated with a SETUP-vs-CONFORMANCE distinction and a diagnostics
trap:

- (a) the Sandbox is ADMITTED by the upstream CRD (verbatim);
- (b) the facade creates the bridged husk-backed `Sandbox` (owner reference
  back to the Sandbox + the `mitos.run/pool` bridge annotation set to the
  default pool);
- (c) the facade UPDATES the Sandbox status with a Ready condition reflecting the
  run-path state achievable on kind (Ready=False while the husk claim is Pending,
  since no VMM boots here);
- (d) deleting the Sandbox GCs the bridged sandbox (the live apiserver runs the
  owner-reference garbage collector);
- (e) an `operatingMode=Suspended` Sandbox terminates the run-path object
  (pause = warm-pool release);
- (f) an `operatingMode` Running->Suspended->Running toggle is an OBJECT-LEVEL
  resume: after the pause releases the claim, setting `operatingMode` back to
  Running RE-ACTIVATES it (the facade re-creates the bridged sandbox). This is the
  object-level half of the pause/resume mapping; the in-VM resume tail (snapshot
  load + resume + guest-ready) is proven on the `cluster-facade-conformance-e2e` job, not
  asserted here.

The job then applies their three extension example manifests UNCHANGED
(`sandboxtemplate.yaml`, `sandboxwarmpool.yaml`, `sandbox-claim.yaml`) and asserts
the extension mappings object-level:

- (g) their `SandboxTemplate` (`secure-datascience-template`) creates our
  `SandboxPool` (carrying the template inline) of the same name;
- (h) their `SandboxWarmPool` (`sandboxwarmpool-example`, replicas 1) creates our
  `SandboxPool` at replicas 1 pointing at the resolved template;
- (i) their `SandboxClaim` (`my-secure-sandbox`, `spec.warmPoolRef.name:
  sandboxwarmpool-example`) binds our `Sandbox` from our pool of that name
  (`sandboxwarmpool-example`), with the `mitos.run/pool` bridge annotation;
- (j) deleting their `SandboxClaim` GCs our `Sandbox` (the live-apiserver
  owner-reference cascade).

The job echoes that the in-VM Ready tail (PodReady / ChromeReady) needs a
KVM-capable kubelet (the bare-metal boundary) and is not asserted there.

## Did we run their Go e2e suite? (honest answer: no, by design)

Their `test/e2e` Go suite is vendored as the conformance REFERENCE and is NOT
run against our facade. This is not laziness; it is a structural mismatch we
verified by reading the vendored sources:

- `test/e2e/framework/context.go` imports `sigs.k8s.io/agent-sandbox/controllers`
  (their in-tree controller package). The framework wires THEIR controller's
  scheme and dumps THEIR controller's pod logs (`app=agent-sandbox-controller`
  in `agent-sandbox-system`). It expects their controller running.
- Every conformance test asserts upstream-controller-created objects that our
  facade does not produce: a `Pod` named after the Sandbox, a headless
  `Service`, and a Sandbox status whose Ready condition is literally
  `Message: "Pod is Ready; Service Exists"`, `Reason: DependenciesReady` (see
  `basic_test.go`). Those predicates require a RUNNING pod that becomes Ready.

Our facade bridges a Sandbox to a husk-backed `Sandbox` on the fork engine;
it does not create a Pod/Service. So their Go SUITE (which wires their in-tree
controller and asserts the literal Pod/Service objects) is bare-metal-gated and
is not run; running it green end to end is a follow-up issue. The GENERIC in-VM
Ready predicate it depends on, however, IS proven independently: the
`cluster-facade-conformance-e2e` job applies the upstream Sandbox unchanged and
asserts it reaches Ready=True on a real booted VMM through the facade (the
PROVEN-ON-KVM rows below), with exec-through liveness. What stays NEEDS-BARE-METAL
is the workload-specific predicates (ChromeReady, python-runtime serving) and the
literal upstream Pod/Service objects their controller creates.

## Conformance matrix

### Upstream `test/e2e/*_test.go` (the Go conformance reference)

| Upstream test | What it asserts upstream | Status | Notes |
| --- | --- | --- | --- |
| `test/e2e/basic_test.go` :: `TestSimpleSandbox` | Sandbox -> Pod Ready + Service, status `"Pod is Ready; Service Exists"` | PROVEN-ON-KVM (Ready predicate) / NEEDS-BARE-METAL (the literal upstream Pod/Service objects) | The Ready predicate is PROVEN-ON-KVM: `cluster-facade-conformance-e2e` applies the upstream Sandbox UNCHANGED and asserts it reaches Ready=True on a real booted VMM through the facade, with exec-through liveness. Object-level admission + the bridged sandbox are also proven on kind (facade-conformance (a),(b)). The literal upstream `Pod`/`Service` objects are created by THEIR controller, not our facade (we bridge to a husk-backed Sandbox), so those specific objects stay NEEDS-BARE-METAL. |
| `test/e2e/replicas_test.go` :: `TestSandboxReplicas` (now `operatingMode`) | Suspended deletes the Pod, keeps the Service | PROVEN-OBJECT-LEVEL-ON-KIND (object) + PROVEN-ON-KVM (in-VM resume tail) / NEEDS-BARE-METAL (Pod/Service objects) | The pause/resume contract is proven against our run-path object: facade-conformance (e) asserts operatingMode=Suspended RELEASES the bridged sandbox and (f) asserts the Running->Suspended->Running toggle RE-ACTIVATES it (object-level resume). The in-VM resume TAIL is PROVEN-ON-KVM: `cluster-facade-conformance-e2e` stage 6 asserts operatingMode Suspended->Running re-activates to Ready on a real VMM. The upstream Pod/Service deletion needs their controller. |
| `test/e2e/shutdown_test.go` :: `TestSandboxShutdownTime`, `TestSandboxRetainedExpiryPreservesFinishedCondition` | shutdown tears down Pod/Service in bounded time; Finished condition retained | NEEDS-BARE-METAL | Requires a running Pod that succeeds and the upstream Finished-condition controller. The deletion/GC object contract is proven object-level (facade-conformance (d)). |
| `test/e2e/parallelism_test.go` :: `TestParallelSandboxes`, `TestParallelSandboxClaimsWith{Sufficient,Insufficient}WarmPool` | many Sandboxes/Claims reach Ready in parallel via a warm pool | NEEDS-BARE-METAL | Waits `ReadyConditionIsTrue` on running sandboxes drawn from a warm pool; needs the in-VM boot and the warm-pool/claim extension mappings (a later slice). |
| `test/e2e/volumeclaimtemplate_test.go` :: `TestSandboxVolumeClaimTemplates` | `volumeClaimTemplates` produce PVCs bound to the Pod | NEEDS-BARE-METAL + JUSTIFIED-EXCEPTION | The facade does not yet map `volumeClaimTemplates` (storage contract, exception 4 below); upstream also needs a running Pod. The manifest still applies unchanged and the claim bridges (proven object-level). |
| `test/e2e/chromesandbox_test.go` :: `TestRunChromeSandbox`, `BenchmarkChromeSandboxStartup` | Chrome serves CDP inside the sandbox; measures PodReady + ChromeReady | NEEDS-BARE-METAL | The canonical running-sandbox predicate (ChromeReady on the CDP port). Pure in-VM boot; the bare-metal boundary. |
| `test/e2e/chromesandbox_claim_test.go` :: `BenchmarkChromeSandboxClaimStartup` | Chrome via a `SandboxClaim` drawn from a warm pool | NEEDS-BARE-METAL | Running sandbox + the warm-pool/claim extension mapping (later slice). |
| `test/e2e/extensions/pythonruntime_test.go` :: `TestRunPythonRuntimeSandbox`, `...Claim`, `...Warmpool` | a python runtime sandbox/claim/warmpool serves requests | NEEDS-BARE-METAL | Running sandbox + the extension mappings (later slice). |
| `test/e2e/extensions/shutdown_policy_test.go` :: `TestSandboxClaim{DeleteForeground,TTL...,ExpiryUsesEarlier...,FinishedWithoutTTLIsRetained,TTLZeroRetain...}` | SandboxClaim TTL / shutdown-policy / finished-condition retention | NEEDS-BARE-METAL + JUSTIFIED-EXCEPTION | The `SandboxClaim` extension is now MAPPED object-level (their claim -> our fork-from-snapshot claim; ttl/shutdownTime mapped, shutdownPolicy a documented exception 5). The TTL/finished-condition timing predicates need a running claim that finishes (the bare-metal boundary). |
| `test/e2e/extensions/warmpool_rollout_test.go` :: `TestWarmPoolRollout`, `...MultiTemplateIsolation`, `...SwitchTemplate`, `...MetadataUpdate` | SandboxWarmPool rollout/template-switch semantics | NEEDS-BARE-METAL + JUSTIFIED-EXCEPTION | The `SandboxWarmPool` extension is now MAPPED object-level (their warm pool -> our pool at replicas). The per-pod rollout/`updateStrategy` semantics are unmapped (exception 3) and the rollout predicates need running pool pods. |
| `test/e2e/extensions/warmpool_sandbox_watcher_test.go` :: `TestWarmPoolSandboxWatcher`, `TestWarmPoolPodNameAnnotationBeforeReady` | warm-pool watcher annotates the bound pod | NEEDS-BARE-METAL + JUSTIFIED-EXCEPTION | `SandboxWarmPool` mapped object-level; the bound-pod annotation predicate needs a running pod (the bare-metal boundary). |
| `test/e2e/extensions/sandboxclaim_metric_test.go` :: `TestSandboxClaimObservabilityAnnotation` | a SandboxClaim observability annotation is set | NEEDS-BARE-METAL + JUSTIFIED-EXCEPTION | `SandboxClaim` mapped object-level; the observability annotation is set by the upstream controller on a running claim (the bare-metal boundary). |
| `test/e2e/framework/watchset_test.go` :: `TestWatchSet...` | the framework's own watch-set unit test | JUSTIFIED-EXCEPTION (not a facade conformance test) | Tests the upstream test framework internals, not the Sandbox API surface; nothing for the facade to satisfy. Vendored for completeness of the reference. |

### Vendored example manifests (apply-unchanged)

Core `agents.x-k8s.io/v1beta1` Sandbox examples: each applies UNCHANGED and the
facade bridges a husk-backed claim object-level (`internal/facade/examples_test.go`
asserts all of these; `facade-conformance` asserts hello-world end to end against
a live apiserver). The fields beyond identity + the first container's env are
JUSTIFIED-EXCEPTIONs (see the exceptions section); the manifest still applies and
reconciles.

| Example manifest | Extra podTemplate fields (exceptions) | Status |
| --- | --- | --- |
| `examples/hello-world-sandbox/hello-world.yaml` | restartPolicy | PROVEN-OBJECT-LEVEL-ON-KIND (envtest + facade-conformance job) |
| `examples/sandbox-ksa/sandbox.yaml` | serviceAccountName, volumeClaimTemplates, volumeMounts, command | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/hermes-agent/sandbox.yaml` | env.valueFrom (secret refs, mirrored through), ports, volumeMounts, volumes, volumeClaimTemplates, args | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/python-runtime-sandbox/sandbox-python-kind.yaml` | ports, imagePullPolicy, podTemplate labels/annotations | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/aio-sandbox/aio-sandbox.yaml` | ports, resources | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/kata-gke-sandbox/sandbox-kata-gke.yaml` | runtimeClassName, resources | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/openclaw-sandbox/openclaw-sandbox.yaml` | ports, env, volumes | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/jupyterlab/jupyterlab.yaml` | ports, resources | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/jupyterlab/jupyterlab-full.yaml` | multiple containers, env.valueFrom, volumeClaimTemplates | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/analytics-tool/jupyterlab.yaml` | ports, resources | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/analytics-tool/analytics-tool/sandbox-python.yaml` | ports | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/langchain/deployment.yaml` (the Sandbox doc) | env, resources (the Deployment docs are not Sandboxes) | PROVEN-OBJECT-LEVEL-ON-KIND (envtest, Sandbox doc only) |
| `examples/vscode-sandbox/base/vscode-sandbox.yaml` | env, ports, volumeClaimTemplates | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/vscode-sandbox/overlays/kata-mshv/patch-kata.yaml` | runtimeClassName (full manifest with containers) | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/policy/kyverno/.chainsaw-tests/setup-sandbox.yaml` | minimal Sandbox fixture | PROVEN-OBJECT-LEVEL-ON-KIND (envtest) |
| `examples/vscode-sandbox/overlays/{kata,gvisor}/patch-kata.yaml`, `patch-gvisor.yaml` | strategic-merge patch fragments (no containers) | JUSTIFIED-EXCEPTION | not standalone applyable Sandboxes; layered onto the base via kustomize upstream. Recorded under the vscode-sandbox base row. |

Extension example manifests (`extensions.agents.x-k8s.io` kinds): the facade now
maps the core `Sandbox` AND all three extension kinds. Each example applies
UNCHANGED and the facade maps it to our corresponding object at the OBJECT level
(`internal/facade/extension_reconciler_test.go` asserts the mappings in envtest;
the `facade-conformance` kind job applies `sandboxtemplate.yaml` +
`sandboxwarmpool.yaml` + `sandbox-claim.yaml` unchanged against a live apiserver
and asserts facts (g)-(j)). The fields beyond the mapped subset are
JUSTIFIED-EXCEPTIONs (see the exceptions section); the manifest still applies and
maps.

| Extension example manifest | Kind | Status |
| --- | --- | --- |
| `extensions/examples/sandboxwarmpool.yaml` | SandboxWarmPool | PROVEN-OBJECT-LEVEL-ON-KIND (envtest + facade-conformance (h)); updateStrategy unmapped (exception 3) |
| `extensions/examples/sandboxtemplate.yaml`, `secure-sandboxtemplate.yaml`, `llm.yaml` | SandboxTemplate | PROVEN-OBJECT-LEVEL-ON-KIND (envtest + facade-conformance (g)); volumeClaimTemplates/networkPolicy/securityContext/ports unmapped (exception 3) |
| `extensions/examples/sandbox-claim.yaml` | SandboxClaim (`warmPoolRef`) | PROVEN-OBJECT-LEVEL-ON-KIND (envtest + facade-conformance (i),(j); lifecycle shutdownTime/shutdownPolicy mapped per exception 5; volumeClaimTemplates the cold-start storage exception) |

## Documented exceptions (justified, not silent)

1. podTemplate fidelity. The facade reconciles the upstream
   `spec.podTemplate.spec.containers[0].env` onto our run path (env.valueFrom
   refs copy through unchanged). Other podTemplate fields (image, resources,
   ports, command/args, volumes/volumeMounts, serviceAccountName, security
   context, multiple containers) are NOT yet honored per-Sandbox: the husk pool
   pins image and resources at pool-build time, and a Sandbox binds to a pool via
   the `mitos.run/pool` bridge annotation. The manifests still apply unchanged
   and the facade bridges the claim. Closing this requires the pool/template
   extension mappings (a later slice).

2. pause/resume semantics. Upstream hibernation is a disk roundtrip (the pod is
   torn down and its state persisted to a volume, then rebuilt). The upstream
   pause/resume contract is the `spec.operatingMode` Running<->Suspended toggle
   (v0.5.0 replaced the v1alpha1 `spec.replicas` 0/1 with this named enum; their
   controller deletes the pod on Suspended and cold-creates a fresh one on
   Running). The facade maps it onto the husk warm pool: `operatingMode=Suspended`
   (pause) RELEASES the bridged sandbox so the bound husk pod returns dormant to
   the warm pool; `operatingMode=Running` after Suspended (resume) RE-ACTIVATES a
   dormant warm husk pod via the same fast path as create (the ~42ms husk
   activation). The conformant observable is preserved: the upstream `Suspended`
   status condition reflects the paused state, the Ready condition reflects
   Paused/Ready honestly, and pause clears `serviceFQDN` + `podIPs` while resume
   re-populates them. This OBJECT-LEVEL behavior is proven on kind (envtest
   `internal/facade` + the `facade-conformance` job's operatingMode
   Running->Suspended->Running resume assertion). The resume-latency advantage (warm
   re-activation vs a cold pod create) is the DESIGN claim; the in-VM
   head-to-head number is a bare-metal-reference-node TARGET, measured by
   `bench/facade/` (see [`../bench/facade/README.md`](../bench/facade/README.md)
   and the "Facade vs upstream reference: resume latency" section of
   `BENCHMARKS.md`). Note our resume is STATE-FRESH (a warm dormant pod, not the
   exact pre-pause in-VM state); a state-PRESERVING pause (a memory snapshot taken
   across the pause via the Checkpoint primitive, so resume restores the exact
   in-VM state) is a documented FUTURE (slice 4), not implemented here. No public
   latency number is stated until the bare-metal harness produces it (the
   no-unverified-claims rule).

3. extension kinds + unmapped fields. The facade now maps all three extension
   kinds object-level: `SandboxTemplate` -> our `SandboxPool` (template inline),
   `SandboxWarmPool` -> our `SandboxPool` (warm), the upstream `SandboxClaim` ->
   our `Sandbox` (source.poolRef). Within
   each mapping the facade maps a faithful subset and lists the rest as
   unmapped (recorded, not silently dropped):
   - SandboxTemplate: the first podTemplate container's `image`, `command`, and
     `env` map onto our template. UNMAPPED (enumerated, not silently dropped):
     `podTemplate.metadata.labels` and `podTemplate.metadata.annotations` (the
     upstream PodTemplate ObjectMeta; our template carries no per-pod metadata
     field); `volumeClaimTemplates`; `networkPolicy` / `networkPolicyManagement`;
     `envVarsInjectionPolicy`; `service`; the pod `securityContext` and the
     container `securityContext`; container `ports` / `volumeMounts`;
     `resources`; `args`; `initContainers`; `serviceAccountName`;
     `runtimeClassName`; and multiple containers. Our husk pool pins resources at
     build time and our engine is fork-from-snapshot, not pod-native, so these
     pod-shaped fields have no our-engine equivalent yet.
   - SandboxWarmPool: `spec.replicas` (re-read every reconcile, so an HPA scaling
     their warm pool propagates) and `sandboxTemplateRef` map onto our pool.
     UNMAPPED: `updateStrategy` (Recreate / OnReplenish). Our husk warm pool
     self-heals dormant slots and rebuilds on a template-snapshot change; we do
     not expose the upstream per-pod rollout knob.
   - SandboxClaim: `warmPoolRef` (see exception 5), `env`, and `lifecycle` (see
     exception 5) map onto our claim; `additionalPodMetadata.annotations` are
     propagated onto our claim as annotations (best-effort traceability). UNMAPPED:
     the per-variable `containerName` env targeting (our run path applies env
     globally into the guest); `additionalPodMetadata.labels` (our claim has no
     per-pod label field); and `volumeClaimTemplates` (the cold-start storage
     contract, the same unmapped storage shape as exception 4).

4. stable identity / storage contract.
   - Stable identity: our claim's run-path `Status.Endpoint` is the bound
     sandbox's serving address; the facade derives `status.sandbox.podIPs` from
     it (the host portion) and the upstream Sandbox reconciler derives the
     `serviceFQDN` consistently from the cluster domain
     (`<name>.<namespace>.svc.<cluster-domain>`). The extension `SandboxClaim`
     status mirrors the bound `sandbox.name` (our claim name) so the upstream
     identity contract is observable. PROVEN-OBJECT-LEVEL-ON-KIND (envtest
     mirrors the bound sandbox name + podIPs).
   - Storage contract: `volumeClaimTemplates` on the upstream `SandboxTemplate`
     are NOT yet mapped onto our template volumes (our `SandboxVolume` shape
     differs and the husk pool provisions storage at build time). JUSTIFIED-
     EXCEPTION: the template still applies unchanged and maps the container
     subset; full volume fidelity is a later slice.

5. warmPoolRef + cold-start + lifecycle (the SandboxClaim mapping).
   - warmPoolRef: v0.5.0 dropped the v1alpha1 `warmpool` policy (none / default /
     named). A `SandboxClaim` now references a `SandboxWarmPool` directly by name
     via `spec.warmPoolRef.name`. The facade binds our `Sandbox` from OUR pool of
     that name (the pool our SandboxWarmPool reconciler created under the same
     name, the bridge), recorded on our claim via `mitos.run/pool`.
     PROVEN-OBJECT-LEVEL-ON-KIND (envtest + facade-conformance (i)). The upstream
     "cold start without pre-warming" idiom (reference a warm pool with
     `replicas: 0`) maps cleanly: our pool of that name simply holds no dormant
     slots, so the claim forks on demand.
   - cold-start fields: upstream forces a cold start when a claim carries `env` or
     `volumeClaimTemplates`. The facade maps `env` onto our run path (mirrored into
     the guest). JUSTIFIED-EXCEPTION: `volumeClaimTemplates` on the claim are NOT
     mapped onto our volumes (the same unmapped storage shape as exception 4); the
     manifest still applies unchanged and the claim binds.
   - Lifecycle: `lifecycle.ttlSecondsAfterFinished` maps onto our claim's
     `Spec.TTLSecondsAfterFinished`; `lifecycle.shutdownTime` (an absolute
     expiry) is recorded on our claim via `mitos.run/shutdown-time` so it is
     not silently dropped. JUSTIFIED-EXCEPTION: `lifecycle.shutdownPolicy`
     (Delete / Retain) governs the UPSTREAM claim object only; our facade enforces
     deletion via the owner-reference cascade (deleting their claim GCs ours) and
     does not separately implement the Retain-vs-Delete distinction at the
     our-claim level.

6. deletion fidelity. Each extension object owns its mapped our-object via a
   controller owner reference, so deleting their `SandboxTemplate` /
   `SandboxWarmPool` / `SandboxClaim` GCs our template / pool / claim
   respectively. PROVEN-OBJECT-LEVEL-ON-KIND: envtest asserts the owner-reference
   linkage (envtest has no GC controller) and facade-conformance (j) asserts the
   live-apiserver GC cascade for the SandboxClaim.

## What is PROVEN now

- envtest (`internal/facade`): the facade creates the bridged husk-backed claim
  for a Sandbox, mirrors its readiness into the Sandbox status, RELEASES the
  claim + clears the serving observables on `operatingMode=Suspended` (pause),
  RE-ACTIVATES the claim on `operatingMode=Running` after Suspended (resume), is
  stable + idempotent under a Running->Suspended->Running->Suspended toggle, and
  leaves the claim owner-referenced for GC on delete. Every vendored core Sandbox
  example applies unchanged and bridges a claim.
- envtest (`internal/facade/extension_reconciler_test.go`): all three extension
  kinds map object-level. Their `SandboxTemplate` creates our template (image /
  command / env mapped, bridge annotation, owner reference); their
  `SandboxWarmPool` creates our pool at the requested replicas (and an upstream
  replica change, as an HPA would make, propagates); their `SandboxClaim` binds
  our claim from the pool named by `spec.warmPoolRef` (with `env` mirrored),
  mirrors a Ready/Bound condition + the bound sandbox name into the upstream claim
  status, and leaves our claim owner-referenced for GC on delete.
- CI (`facade-conformance` kind job): their hello-world Sandbox applies UNCHANGED
  against a live apiserver and the object-level facts (a)-(f) above hold,
  including the operatingMode Running->Suspended->Running OBJECT-LEVEL resume (the
  facade releases then re-creates the bridged sandbox). Their three extension
  example manifests apply UNCHANGED and the object-level facts (g)-(j) hold (their
  template/warmpool/claim map to our template/pool/claim; deletion GCs ours).
- CI (`cluster-facade-conformance-e2e` on the real KVM cluster): the upstream
  Sandbox applied UNCHANGED reaches the in-VM Ready predicate ("Pod is Ready;
  Service Exists" analog) on a real booted Firecracker VMM through the facade,
  exec-through the bridged sandbox returns the expected stdout (in-VM liveness),
  and the operatingMode Suspended->Running resume tail re-activates to Ready. This
  is the PROVEN-ON-KVM half of the matrix: the GENERIC in-VM Ready predicate and
  the in-VM resume tail.
- `bench/facade/`: the reproducible pause/resume latency harness + methodology
  (object-level resume on kind; the in-VM head-to-head a bare-metal target).

## What is OPEN

- ChromeReady and the python-runtime serving check: the WORKLOAD-SPECIFIC in-VM
  predicates (Chrome serving CDP, the python runtime serving requests) are not run
  on `cluster-facade-conformance-e2e` (the GENERIC Ready predicate is proven
  there). Running those exact workloads is a follow-up; they stay NEEDS-BARE-METAL.
- The literal upstream `Pod`/`Service` objects: their in-tree controller creates a
  Pod + headless Service per Sandbox; our facade bridges to a husk-backed Sandbox
  instead, so those specific objects are never produced (a documented difference,
  not a regression).
- Running the full upstream Go e2e suite green end to end (needs their controller
  + the running-sandbox tail). Follow-up issue.
- The latest-two-minors CI matrix (only v0.5.0 is wired now; the second minor is
  a follow-up issue).
- State-PRESERVING pause: a memory snapshot taken across the pause (the
  Checkpoint primitive) so resume restores the exact pre-pause in-VM state, not a
  fresh warm pod. The object-level pause/resume mapping (warm-pool release + fast
  re-activation) and the `bench/facade/` methodology are DONE; the in-VM
  head-to-head resume number stays a bare-metal target.
- Full podTemplate fidelity (image/resources/ports/volumeMounts honored
  per-Sandbox) and the upstream `volumeClaimTemplates` storage contract mapped
  onto our template volumes. (The booted in-VM serving endpoint itself is now
  PROVEN-ON-KVM via `cluster-facade-conformance-e2e`; what remains open is mapping
  the per-Sandbox pod-shaped fields and the volume storage contract.)

The facade now maps the core `Sandbox` and all three extension kinds
(`SandboxTemplate`, `SandboxWarmPool`, `SandboxClaim`) object-level; the
extension mappings listed as OPEN in earlier slices are DONE.
