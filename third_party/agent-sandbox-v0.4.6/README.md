# Vendored upstream: sigs.k8s.io/agent-sandbox (the previous minor)

These are vendored upstream artifacts from the SIG agent-sandbox project, copied
VERBATIM from the module cache for `sigs.k8s.io/agent-sandbox@v0.4.6`. They are
NOT our work; the upstream Apache 2.0 `LICENSE` is preserved alongside them. We
do NOT edit these manifests: applying them unchanged is the whole point of the
conformance harness (issue #19).

This directory is the SECOND pinned minor under the latest-two-minors policy
(issue #506). The current stable minor (`v0.5.0`, the `v1beta1` graduated API)
is vendored under `../agent-sandbox` and carries the full object-level bridging
conformance. This tree pins the PREVIOUS minor, `v0.4.6`, whose surface is the
`agents.x-k8s.io/v1alpha1` API (v0.5.0 graduated the API to `v1beta1`; v0.4.6
serves only `v1alpha1`, with no conversion webhook). See
docs/facade-conformance.md for why the two minors are tracked and what each
asserts.

## What this pin asserts (and what it does not)

The facade (`cmd/facade` + `internal/facade`) imports and serves the graduated
stable surface `agents.x-k8s.io/v1beta1`. It does NOT serve `v1alpha1`. So the
v0.4.6 (`v1alpha1`) artifacts are tracked for APPLY-UNCHANGED conformance:

- the vendored `v1alpha1` CRDs install cleanly, and
- the vendored `v1alpha1` example manifests apply UNCHANGED and are ADMITTED by
  those CRDs.

The deeper bridging facts (the facade creating the husk-backed run-path object,
the mirrored status, the operatingMode pause/resume, the extension mappings) are
asserted only for the `v1beta1` minor and are a JUSTIFIED EXCEPTION for this
`v1alpha1` minor (the facade targets the graduated stable surface; `v1alpha1` is
the deprecated previous-minor surface). This is recorded in the conformance
matrix, not hidden. See docs/facade-conformance.md.

## Version

- Module: `sigs.k8s.io/agent-sandbox`
- Version: `v0.4.6` (pinned, the latest patch of the previous minor v0.4)
- API: group `agents.x-k8s.io`, version `v1alpha1` (core `Sandbox`); group
  `extensions.agents.x-k8s.io`, version `v1alpha1` (the extension kinds:
  `SandboxWarmPool`, `SandboxTemplate`, `SandboxClaim`).

## Contents

- `crds/`: the upstream CRDs (copied from the upstream `k8s/crds/`): the core
  `Sandbox` CRD plus the three extension CRDs.
- `examples/`: the full upstream `examples/` tree, including the core `Sandbox`
  example manifests (apply-unchanged admission targets).
- `extensions/examples/`: the upstream extension example manifests
  (`SandboxWarmPool`, `SandboxTemplate`, `SandboxClaim`).
- `go.mod`: a nested-module marker so the parent module's `go build` / `go vet` /
  `go test ./...` skip this subtree (the upstream examples tree carries one
  build-tagged Go file that must never compile here). Do NOT add it to a
  `go.work` or run `go mod tidy` against it.
- `LICENSE`: the upstream Apache 2.0 license, preserved.

## Updating

Bump the pinned patch above (staying on the v0.4 minor while it remains one of
the latest two upstream minors), then re-copy from the module cache:

```bash
UP="$(go env GOMODCACHE)/sigs.k8s.io/agent-sandbox@<version>"
cp "$UP/k8s/crds/"*.yaml crds/
cp -R "$UP/examples" examples
cp -R "$UP/extensions/examples" extensions/examples
cp "$UP/LICENSE" LICENSE
chmod -R u+w crds examples extensions LICENSE
```

(The module cache is read-only, hence the `chmod`.) When upstream ships a newer
minor, this tree should be re-pinned to the new second-latest minor (or retired
if the API surface it covers leaves the latest-two window). Re-run the facade
envtest suite and update the conformance matrix in docs/facade-conformance.md.
