# Releasing Mitos

This page is the one place that documents every published artifact: the
container images, the CLI, and the four language SDKs. It records what each
release path publishes, what triggers it, and the one-time maintainer setup each
needs.

Honesty rule (CLAUDE.md): a path is only described as live once it actually
publishes. The SDK registry paths go live the first time a maintainer wires the
matching secret and bumps an SDK version; until then the publish job runs and
skips with a clear log.

## What ships, and how

| Artifact | Registry | Workflow | Trigger |
| --- | --- | --- | --- |
| Container images (controller, forkd, husk-stub, kvm-device-plugin) | ghcr.io/mitos-run, cosign-signed, SBOM-attested | `.github/workflows/publish.yaml` | `v*` tag push, or manual dispatch with a tag input |
| CLI (`mitos`): binaries, archives, checksums, deb/rpm, Homebrew cask | GitHub Releases plus the Homebrew tap | `.github/workflows/release.yaml` (goreleaser) | `v*` tag push, or manual dispatch with a `ref` input |
| kubectl plugin (`kubectl mitos`) | krew-index | `.github/workflows/krew.yaml` (`.krew.yaml` template) | `release: published`, or manual dispatch ON the tag ref |
| OLM operator bundle | OperatorHub.io (`k8s-operatorhub/community-operators`) | `.github/workflows/operator-release.yaml` | `release: published`, or manual dispatch with `version`/`previous` inputs |
| Python SDK (dist `mitos-run`, import `mitos`) | PyPI | `.github/workflows/publish-sdks.yaml` | version bump in `sdk/python/pyproject.toml` merged to main |
| TypeScript SDK (`@mitos/sdk`) | npm | `.github/workflows/publish-sdks.yaml` | version bump in `sdk/typescript/package.json` merged to main |
| Ruby SDK (`mitos`) | RubyGems | `.github/workflows/publish-sdks.yaml` | version bump in `sdk/ruby/lib/mitos/version.rb` merged to main |
| Rust SDK (`mitos`) | crates.io | `.github/workflows/publish-sdks.yaml` | version bump in `sdk/rust/Cargo.toml` merged to main |

The Python distribution publishes as `mitos-run` because the bare `mitos` name on
PyPI is taken by an unrelated project; the import package stays `mitos`, so users
`pip install mitos-run` and `import mitos`.

The container image and CLI versions are driven by release-please, which tracks
the root Go module only. The four SDK versions are independent and bumped by
hand in each SDK's own manifest.

## Publishing an SDK

The flow is the same for all four SDKs:

1. Bump the version in the SDK's manifest:
   - Python: `version` in `sdk/python/pyproject.toml`
   - TypeScript: `version` in `sdk/typescript/package.json`
   - Ruby: `VERSION` in `sdk/ruby/lib/mitos/version.rb`
   - Rust: `version` in `sdk/rust/Cargo.toml`
2. Merge the change to main.
3. The push to main matches the SDK's path filter in
   `.github/workflows/publish-sdks.yaml` and runs that SDK's job. The other SDK
   jobs do not run.

You can also trigger a publish by hand from the Actions tab using the
"Publish SDKs" workflow's `workflow_dispatch`. Its `sdk` input forces one SDK
(`python`, `typescript`, `ruby`, `rust`) or `all`.

### Idempotent skip-if-exists

Every SDK job follows the same safe sequence:

1. Read the version the SDK declares in its manifest.
2. Ask the registry whether that exact version already exists. If it does, the
   job logs "already published" and exits successfully without uploading.
3. The token-based jobs (npm, RubyGems) additionally skip when their secret is
   empty, logging "token not configured" and exiting successfully without
   uploading. This mirrors the Homebrew-tap token skip in `release.yaml`, so the
   workflow never hard-fails before the secrets are wired. The OIDC jobs (PyPI,
   crates.io) need no secret.
4. Otherwise it builds and publishes that version.

Because of steps 2 and 3, re-running the workflow or pushing an unrelated change
to an SDK directory never errors on a duplicate upload. The publish only happens
once per version, when the version is new and (for the token-based jobs) the
token is present.

The version-existence checks per registry:

| SDK | Existence check |
| --- | --- |
| Python | HTTP 200 from `https://pypi.org/pypi/mitos-run/<version>/json` |
| TypeScript | non-empty output from `npm view @mitos/sdk@<version> version` |
| Ruby | the version appears in `https://rubygems.org/api/v1/versions/mitos.json` |
| Rust | HTTP 200 from `https://crates.io/api/v1/crates/mitos/<version>` |

The Rust job additionally guards on `sdk/rust/Cargo.toml` existing; if the
manifest is absent (for example before that SDK lands) the job no-ops cleanly.

## One-time maintainer setup

PyPI and crates.io authenticate via Trusted Publishing (OIDC), so they need no
Actions secret. The npm and RubyGems jobs stay dormant (logging "token not
configured") until a maintainer adds their token. Do the steps below once.

### Reserve the names

Reserve these names before the first publish so they cannot be taken:

- PyPI project: `mitos-run` (the import package stays `mitos`; the bare `mitos`
  name on PyPI is taken by an unrelated project)
- npm package: `@mitos/sdk` (the `@mitos` scope must exist and be public)
- RubyGems gem: `mitos`
- crates.io crate: `mitos`

### How each registry authenticates

| Registry | Auth | Secret |
| --- | --- | --- |
| PyPI | Trusted Publishing (OIDC) | none |
| crates.io | Trusted Publishing (OIDC), after a one-time token-based first publish | none (after bootstrap) |
| npm | Automation access token | `NPM_TOKEN` |
| RubyGems | API key with the "Push rubygem" scope | `RUBYGEMS_API_KEY` |

### Tokens to add as Actions secrets

Add each token as a repository (or organization) Actions secret with the exact
name below. Only npm and RubyGems still need a token; PyPI and crates.io are
tokenless via OIDC.

| Secret | Registry | How to mint it |
| --- | --- | --- |
| `NPM_TOKEN` | npm | Create the `@mitos` org/scope, then an Automation access token (Access Tokens -> Generate -> Automation). |
| `RUBYGEMS_API_KEY` | RubyGems | Create a RubyGems account, then an API key with the "Push rubygem" scope (Profile -> API keys). |

When a secret is present and the SDK's declared version is new, the next merge
that touches that SDK (or a manual dispatch) publishes it.

### PyPI Trusted Publishing (OIDC)

The Python job publishes via Trusted Publishing: the `python` job carries
`id-token: write`, and `pypa/gh-action-pypi-publish` mints a short-lived OIDC
token per run, so there is no long-lived secret to rotate. The PyPI project
`mitos-run` already has a trusted publisher configured for this repository and
the `publish-sdks.yaml` workflow, so no further maintainer action is needed.

To recreate it from scratch: on PyPI, add a trusted publisher to the `mitos-run`
project (or a pending publisher before the project exists) pointing at owner
`mitos-run`, repo `mitos`, workflow `publish-sdks.yaml`.

### crates.io Trusted Publishing (OIDC)

crates.io cannot pre-register a Trusted Publisher for a crate that does not exist
yet, so the very first publish of the `mitos` crate is a one-time, token-based
step done by hand:

1. Mint a temporary crates.io API token with the publish scope (Account settings
   -> API tokens) and run `cargo login <temporary-token>`.
2. From `sdk/rust`, run `cargo publish` to claim the `mitos` crate name.
3. In the crate settings on crates.io, add a GitHub Trusted Publisher with owner
   `mitos-run`, repo `mitos`, and workflow `publish-sdks.yaml`.
4. Revoke the temporary token.

After that bootstrap, every later release uses the OIDC path: the `rust` job
carries `id-token: write` and runs `rust-lang/crates-io-auth-action`, which mints
a short-lived token (its `token` output is passed to `cargo publish` via
`CARGO_REGISTRY_TOKEN`) and revokes it after the job. No `CARGO_REGISTRY_TOKEN`
secret is stored.

## Cutting a release (images, CLI, krew, operator)

release-please opens and lands a `chore(main): release X` PR, then tags the
commit `vX` and publishes a GitHub release for it. From there the four
release-driven channels publish.

### The `RELEASE_PLEASE_TOKEN` is what makes the cascade work

GitHub deliberately blocks events created with the default `GITHUB_TOKEN` (a tag
push, a published release) from triggering other workflows, to avoid recursive
runs. release-please uses whatever token it is given:

- With `RELEASE_PLEASE_TOKEN` set (a PAT or GitHub App token with `contents:write`
  and `workflow`), the tag and release events release-please creates DO cascade,
  so `publish.yaml` (images, on the `v*` tag), `release.yaml` (CLI, on the `v*`
  tag), `krew.yaml` and `operator-release.yaml` (both on `release: published`) all
  fire automatically. This is the intended path.
- Without it, release-please falls back to `GITHUB_TOKEN`, none of those
  downstream workflows fire on their own, and only the explicit dispatch step
  inside `release-please.yml` publishes the images. The CLI, krew, and operator
  then never publish until dispatched by hand. Set the token.

### Required release secrets

| Secret | Needed for | Scopes / notes |
| --- | --- | --- |
| `RELEASE_PLEASE_TOKEN` | the whole release cascade (images, CLI, krew, operator) | classic PAT `repo` + `workflow`, or a GitHub App token with `contents:write` + `workflow`. Without it nothing downstream auto-publishes. |
| `COMMUNITY_OPERATORS_TOKEN` | the OperatorHub PR to `k8s-operatorhub/community-operators` | classic PAT `repo` AND `workflow`. The `workflow` scope is REQUIRED: the PR branch is based on community-operators `main`, which carries `.github/workflows/*`, and GitHub refuses a PAT push touching workflow files without it. The token's account owns the fork the PR comes from. |
| `HOMEBREW_TAP_TOKEN` | the Homebrew cask in `release.yaml` | token scoped to `mitos-run/homebrew-tap`. Optional: when absent the CLI release still succeeds and skips the cask. |
| GHCR write token | image push in `publish.yaml` | documented inline in that workflow; falls back to `GITHUB_TOKEN`. |

OperatorHub also needs a one-time fork of `k8s-operatorhub/community-operators`
under the `COMMUNITY_OPERATORS_TOKEN` account; the workflow creates it on first
run. New images push to GHCR private by default, so making each new image package
public is a one-time manual step. See [docs/distribution.md](distribution.md),
[docs/operatorhub.md](operatorhub.md), and [docs/krew.md](krew.md) for the
per-channel runbooks, and [docs/install.md](install.md) for the install side.

### Manual backfill

When a release was cut before a channel was wired up (or a channel run failed),
backfill an existing tag with these dispatches:

| Channel | Command |
| --- | --- |
| Images | `gh workflow run publish.yaml -f tag=vX.Y.Z -f ref=vX.Y.Z` |
| CLI | `gh workflow run release.yaml -f ref=vX.Y.Z` |
| krew | `gh workflow run krew.yaml --ref vX.Y.Z -f tag=vX.Y.Z` |
| OperatorHub | `gh workflow run operator-release.yaml -f version=X.Y.Z -f previous=<last published>` |

The krew backfill MUST dispatch ON the tag ref (`--ref vX.Y.Z`), not just pass the
tag input: the krew-release-bot reads `GITHUB_REF` and requires a `refs/tags/...`
ref, and a step-level env override does not reach the container action. Because
the bot reads `.krew.yaml` from the dispatched ref, a krew template fix only takes
effect for tags that carry it; krew-index tracks the latest version, so an older
tag that cannot be backfilled is superseded by the next release.
