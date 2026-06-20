# Migration runbook: move mitos to a dedicated GitHub org

Status: prepared, not executed. This runbook moves the engine repo from
`github.com/paperclipinc/mitos` to a dedicated GitHub org, stands up the OSS
website, and adopts the `mitos.run/mitos` Go vanity import path so the org
hosting the code can change without ever churning imports again.

Two pieces are already done and verified on branches:

1. The Go module-path rename to `mitos.run/mitos` is committed on the engine
   branch `chore/migrate-module-path-mitos-run` (see Part 2).
2. The OSS website (Astro + Starlight) with the go-get vanity wiring is a
   separate repo at `../mitos-website`, build-verified (see Part 3).

What is left is account and infrastructure work that cannot be scripted from the
repo: creating the org, transferring the repo, moving images, and re-wiring CI.

## The one ordering rule that matters

`go get mitos.run/mitos` only works once the vanity meta at
`https://mitos.run/mitos?go-get=1` is live and points at the real repo. So:

> Deploy the website (Part 3) and confirm `go get` resolves BEFORE merging the
> module-path rename (Part 2) to `main`. If you merge the rename first, every
> `go get mitos.run/mitos` fails until the meta is up.

Recommended sequence: Part 1 (org) -> Part 4 (transfer repo) -> Part 5 (images)
-> Part 6 (CI) -> Part 3 (website live + verify) -> merge Part 2 (rename).

## Part 0: decisions to make first

- **Org name.** `mitos` may be taken (common word). Fallbacks: `mitos-run`,
  `mitoshq`, `getmitos`. The scaffold currently assumes `mitos-run`; if you pick
  something else, update the three places in Part 3.
- **Ownership.** The org can be owned by Paperclip Inc; you do not need to settle
  entity structure now. A dedicated org is worth it purely for clean diligence
  optionality later.
- **Vanity host.** `mitos.run` must be able to serve static content with rewrite
  rules (Cloudflare Pages, Netlify, or Vercel). The website repo ships configs
  for all three.

## Part 1: create the org

1. Create the GitHub org (the chosen name from Part 0).
2. Set org-level: default branch protection policy, member roles, a team for
   security reviewers (needed for CODEOWNERS in Part 6).
3. Enable GitHub Container Registry for the org (Part 5).

## Part 2: module-path rename (DONE on a branch)

Branch: `chore/migrate-module-path-mitos-run`. What it changed (pure mechanical,
no behavior change):

- `go.mod`: `module github.com/paperclipinc/mitos` -> `module mitos.run/mitos`.
- All 417 `.go` files: import paths rewritten and re-sorted with `gofmt`.
- `proto/forkd.proto`: `option go_package` updated; `Makefile` protoc
  `--go_opt=module=` / `--go-grpc_opt=module=` updated; stubs regenerated with
  `make proto`.
- deepcopy + CRD manifests regenerated with `make generate manifests` (output is
  byte-identical to `main`; the rename does not touch CRD YAML).

What it deliberately did NOT change (these are web/registry references tied to
the GitHub org name, handled in Parts 4 and 5):

- `CHANGELOG.md` (historical release links; leave as-is or let release-please
  manage going forward).
- `README.md` badges and links.
- `ghcr.io/paperclipinc/...` image paths (`Makefile`, `SECURITY.md`,
  `BENCHMARKS.md`, `deploy/`).

### How it was verified locally (CI-equivalent for a string rename)

- `gofmt -l` clean; `go vet ./...` clean.
- `golangci-lint run --timeout=5m` AND `GOOS=linux golangci-lint run` both clean.
- `go build ./...` and `GOOS=linux GOARCH=amd64 go build ./guest/agent/` OK.
- `make proto` and `make generate manifests` OK; CRD output identical to `main`.
- `go test ./...` passes. Python SDK: 77 passed.
- Note: the controller envtest has a PRE-EXISTING flake
  (`TestHuskClaimWorkspaceDiffDelegates` and sibling workspace tests) caused by a
  revision-naming race on the shared envtest apiserver. It was reproduced on
  pristine `main` (1 of 5 runs) and on the rename branch; it is independent of
  the rename. Worth its own issue, but not a blocker for this migration.

### Merge gate

Do NOT merge this branch until the vanity meta is live (Part 3) and points at the
new repo home, and the repo has been transferred (Part 4) so the meta URL is
correct.

## Part 3: stand up the website + vanity import (repo DONE)

Repo: `../mitos-website` (Astro + Starlight), already `git init`'d and committed,
`npm run build` verified, vanity meta confirmed in `dist/mitos/index.html`.

1. Push `mitos-website` to the new org (e.g. `<org>/website` or `<org>/mitos.run`).
2. **Confirm the source repo URL** in three files if the org name is not
   `mitos-run`: `public/mitos/index.html` (the `go-import` and `go-source`
   meta), `astro.config.mjs` (the GitHub social link), and `README.md`.
3. Deploy to the chosen host with the shipped rewrite config:
   - Cloudflare Pages / Netlify: `public/_redirects`.
   - Vercel: `vercel.json`.
   The host MUST serve the meta with HTTP 200 for `/mitos` and every `/mitos/*`
   subpath (rewrite, not redirect).
4. Point the `mitos.run` apex (or a dedicated path) at the deploy.
5. Verify end to end:
   ```bash
   curl -s "https://mitos.run/mitos?go-get=1" | grep go-import
   # expect: <meta name="go-import" content="mitos.run/mitos git https://github.com/<org>/mitos">
   GOPROXY=direct go install mitos.run/mitos/cmd/mitos@latest
   ```

Keep the website OSS. The hosted-service console, billing, and any secrets live
in a SEPARATE private repo.

## Part 4: transfer the engine repo

Prefer GitHub's repo transfer over create-new-and-push: it preserves issues,
PRs, stars, and sets up automatic redirects from the old path.

1. Transfer `paperclipinc/mitos` -> `<org>/mitos` (Settings -> Danger Zone ->
   Transfer). Old git remotes, issue links, and PR links redirect automatically.
2. Update local remotes: `git remote set-url origin git@github.com:<org>/mitos.git`.
3. Note: redirects cover git/issues/PRs/stars; they do NOT cover hardcoded
   references (Parts 5 and 7) or external package/registry consumers.

If you cannot transfer (e.g. you want to keep the old repo), create a new repo in
the org and push; you lose automatic redirects and must announce the move.

## Part 5: move container images and registry references

Images today: `ghcr.io/paperclipinc/mitos-controller`,
`ghcr.io/paperclipinc/mitos-forkd`, `ghcr.io/paperclipinc/mitos-ci-runner`,
`ghcr.io/paperclipinc/sandbox-base-python`.

1. Rebuild/push (or re-tag and push) each under `ghcr.io/<org>/...`.
2. Update references:
   - `Makefile` (`IMG_CONTROLLER`, `IMG_FORKD`).
   - `deploy/` manifests (controller, daemon, `deploy/ci-runner/`).
   - `SECURITY.md`, `BENCHMARKS.md`.
   - the `cosign` keyless signing identity / SBOM attestation subject (it encodes
     the repo path).
3. Set the new packages' visibility (public for the engine images).
4. Pre-launch nobody depends on the old image paths, so no compatibility shim is
   needed; if any external user already pulls them, keep the old tags pinned and
   add a deprecation note.

## Part 6: re-wire CI/CD in the new org

GitHub redirects do NOT carry CI configuration. Re-establish:

1. **Branch protection + required checks** on `main`: re-add the required checks
   (go-test, go-lint, python-test, docker-build, kind-e2e, firecracker-test) and
   "require branches up to date".
2. **CODEOWNERS + reviewers**: the security-review-required-paths policy depends
   on reviewer **org membership**. Create the reviewers team and add members
   before merging anything touching `internal/fork`, `internal/firecracker`,
   `internal/daemon`, `guest/agent`, or token code.
3. **Org/repo secrets**: re-provision ghcr push creds, the cosign/OIDC identity,
   and any release tokens. OIDC subject strings change with the org/repo path;
   update any trust policies that pin them.
4. **Self-hosted bare-metal KVM CI runner** (`deploy/ci-runner/`): it is
   registered to `https://github.com/paperclipinc/mitos`
   (`deploy/ci-runner/deployment.yaml`, the `GITHUB` URL env) with a registration
   Secret. Re-register against the new repo URL, mint a new registration token,
   move the runner image to the new ghcr, and re-apply. Confirm the runner shows
   online and the `cluster-e2e` workflow targets it.
5. **release-please / autorelease**: update any config that encodes the repo
   path; the CHANGELOG compare links will start using the new path going forward.
6. Re-run the full pipeline on a no-op PR to confirm all required checks pass in
   the new org. This is the real "CI green" confirmation for Part 2.

## Part 7: web URL and doc cleanup

- README badges (CI, release, license, Go report) -> new org path.
- README links, `docs/` deep links, `docs/adr/0001-facade-and-naming.md`.
- Leave historical `CHANGELOG.md` links alone (they point at real historical
  commits/issues that the transfer redirect still resolves); let release-please
  use the new path for future entries.
- Repo description and topics on the new org.

## Part 8: final verification checklist

- [ ] `https://mitos.run/mitos?go-get=1` returns the correct `go-import` meta.
- [ ] `GOPROXY=direct go install mitos.run/mitos/cmd/mitos@latest` succeeds.
- [ ] New-org CI: all required checks green on a no-op PR.
- [ ] Self-hosted KVM runner online; `cluster-e2e` green.
- [ ] `docker pull ghcr.io/<org>/mitos-controller` (and forkd) succeed; images
      signed under the new identity.
- [ ] Old `paperclipinc/mitos` URLs redirect to the new org.
- [ ] Module-path rename branch merged AFTER the vanity meta is live.
- [ ] README badges and links resolve.

## Rollback

- Module rename: revert the `chore/migrate-module-path-mitos-run` merge; imports
  return to `github.com/paperclipinc/mitos`. Harmless if the vanity meta is also
  taken down.
- Repo transfer is reversible by transferring back, but avoid thrashing; decide
  the org name once (Part 0) before transferring.
- Vanity meta: if `go get` breaks, the fastest fix is to confirm the host serves
  `/mitos*` with 200 and that the `go-import` repo URL exactly matches the repo
  home.
