# Releasing mitos

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
| CLI (`mitos`): binaries, archives, checksums, deb/rpm, Homebrew cask | GitHub Releases plus the Homebrew tap | `.github/workflows/release.yaml` (goreleaser) | `v*` tag push |
| Python SDK (`mitos`) | PyPI | `.github/workflows/publish-sdks.yaml` | version bump in `sdk/python/pyproject.toml` merged to main |
| TypeScript SDK (`@mitos/sdk`) | npm | `.github/workflows/publish-sdks.yaml` | version bump in `sdk/typescript/package.json` merged to main |
| Ruby SDK (`mitos`) | RubyGems | `.github/workflows/publish-sdks.yaml` | version bump in `sdk/ruby/lib/mitos/version.rb` merged to main |
| Rust SDK (`mitos`) | crates.io | `.github/workflows/publish-sdks.yaml` | version bump in `sdk/rust/Cargo.toml` merged to main |

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
3. If the registry token secret is empty, the job logs "token not configured"
   and exits successfully without uploading. This mirrors the Homebrew-tap token
   skip in `release.yaml`, so the workflow never hard-fails before the secrets
   are wired.
4. Otherwise it builds and publishes that version.

Because of steps 2 and 3, re-running the workflow or pushing an unrelated change
to an SDK directory never errors on a duplicate upload. The publish only happens
once per version, when the version is new and the token is present.

The version-existence checks per registry:

| SDK | Existence check |
| --- | --- |
| Python | HTTP 200 from `https://pypi.org/pypi/mitos/<version>/json` |
| TypeScript | non-empty output from `npm view @mitos/sdk@<version> version` |
| Ruby | the version appears in `https://rubygems.org/api/v1/versions/mitos.json` |
| Rust | HTTP 200 from `https://crates.io/api/v1/crates/mitos/<version>` |

The Rust job additionally guards on `sdk/rust/Cargo.toml` existing; if the
manifest is absent (for example before that SDK lands) the job no-ops cleanly.

## One-time maintainer setup

The SDK publish paths stay dormant (logging "token not configured") until a
maintainer completes the steps below for each registry. Do them once.

### Reserve the names

Reserve these names before the first publish so they cannot be taken:

- PyPI project: `mitos`
- npm package: `@mitos/sdk` (the `@mitos` scope must exist and be public)
- RubyGems gem: `mitos`
- crates.io crate: `mitos`

### Create accounts and tokens, then add the secrets

Add each token as a repository (or organization) Actions secret with the exact
name below.

| Secret | Registry | How to mint it |
| --- | --- | --- |
| `PYPI_API_TOKEN` | PyPI | Create a PyPI account, then an API token scoped to the `mitos` project (Account settings -> API tokens). |
| `NPM_TOKEN` | npm | Create the `@mitos` org/scope, then an Automation access token (Access Tokens -> Generate -> Automation). |
| `RUBYGEMS_API_KEY` | RubyGems | Create a RubyGems account, then an API key with the "Push rubygem" scope (Profile -> API keys). |
| `CARGO_REGISTRY_TOKEN` | crates.io | Sign in to crates.io with GitHub, then create an API token (Account settings -> API tokens) with the publish scope. |

When a secret is present and the SDK's declared version is new, the next merge
that touches that SDK (or a manual dispatch) publishes it.

### Recommended: PyPI Trusted Publishing (OIDC)

Once the PyPI `mitos` project exists, the recommended hardening is to switch the
Python job from an API token to Trusted Publishing (OIDC). Configure the project
on PyPI to trust this repository and the `publish-sdks.yaml` workflow, then in
the Python job add `id-token: write` to its `permissions`, remove the
`password:` line from the publish step, and delete the `PYPI_API_TOKEN` secret.
`pypa/gh-action-pypi-publish` then mints a short-lived OIDC token per run, so
there is no long-lived secret to rotate. The job's comments point to the exact
lines to change.

## Cutting an image or CLI release

Image and CLI releases follow the release-please flow:

1. release-please opens and lands a `chore(main): release X` PR and tags the
   commit `vX`.
2. The `v*` tag drives `release.yaml` (goreleaser builds the CLI binaries,
   archives, checksums, deb/rpm, and, when `HOMEBREW_TAP_TOKEN` is present, the
   Homebrew cask).
3. The signed images are published by dispatching `publish.yaml` with the tag
   input (the release-please tag push does not auto-trigger `on: push: tags`).

The image and CLI maintainer setup (the `HOMEBREW_TAP_TOKEN` and the GHCR write
token) is documented inline in those two workflows and in
[docs/install.md](install.md).
