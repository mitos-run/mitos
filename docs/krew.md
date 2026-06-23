# Distributing kubectl mitos via krew

The `kubectl-mitos` plugin (source: `cmd/kubectl-mitos/main.go`) is
distributed through [krew](https://krew.sigs.k8s.io), the kubectl plugin
manager. The krew plugin name is `mitos`; once installed it is invoked as
`kubectl mitos <subcommand>` (ls, ps, tree, top, logs, exec).

Two files drive distribution:

- `.krew.yaml` at the repo root: the krew Plugin manifest template, written for
  the `rajatjindal/krew-release-bot` action. Its `version`, archive URIs, and
  per-platform sha256 are filled in from the GitHub Release at submission time.
- `.github/workflows/krew.yaml`: on a published release it cross-compiles the
  plugin for the five krew-supported platforms, packages and uploads the
  archives as release assets, then runs the bot to open the krew-index PR.

## Release flow

1. A release is published (release-please cuts the tag and GitHub Release).
2. `.github/workflows/krew.yaml` runs on the `release: published` event:
   - builds `kubectl-mitos` with `CGO_ENABLED=0` for
     linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64;
   - packages each as `kubectl-mitos_<tag>_<os>_<arch>.tar.gz`
     (`.zip` for windows), with the binary at the archive root plus `LICENSE`;
   - uploads the archives as assets onto the release;
   - runs `rajatjindal/krew-release-bot@v0.0.46`, which reads `.krew.yaml`,
     computes the sha256 of each uploaded archive, and opens (or updates) a PR
     against `kubernetes-sigs/krew-index`.
3. After that PR merges, `kubectl krew upgrade mitos` reaches users.

You can re-run the asset build and bot manually with the `workflow_dispatch`
input set to an existing release tag (for example `v0.4.0`).

## Local testing before submission

Build the archive for your platform, then install straight from the manifest.
For example on darwin/arm64 (adjust the os/arch and extension to match):

```bash
TAG=v0.4.0
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 \
  go build -ldflags "-s -w" -o kubectl-mitos ./cmd/kubectl-mitos/
cp LICENSE LICENSE
tar -czf "kubectl-mitos_${TAG}_darwin_arm64.tar.gz" kubectl-mitos LICENSE

kubectl krew install \
  --manifest=.krew.yaml \
  --archive="kubectl-mitos_${TAG}_darwin_arm64.tar.gz"
kubectl mitos ls
```

When installing from a local `--archive`, krew skips the URI download and sha256
check, so the Go-template fields in `.krew.yaml` do not need to resolve for this
test. To validate that the manifest itself parses and the selectors are sound,
also run:

```bash
kubectl krew install --manifest=.krew.yaml --archive=<archive>
```

against the archive for the platform you are on.

## First submission to krew-index is manual

The krew-release-bot automates version bumps after the plugin already exists in
krew-index, but the FIRST listing is a manual pull request you open against
[github.com/kubernetes-sigs/krew-index](https://github.com/kubernetes-sigs/krew-index).

One-time prerequisites krew-index requires:

- The plugin must already be released, so the archive URLs in the manifest
  resolve to real, downloadable assets with stable sha256.
- The manifest must validate with `kubectl krew install --manifest=...`
  (the local-archive form above) on at least one platform.
- The manifest lives at `plugins/mitos.yaml` in krew-index, with the
  template fields resolved to concrete values for the released tag. The
  simplest way to produce that resolved file is to let the workflow run once on
  a real release and copy the manifest the bot generates, or render it by hand
  from `.krew.yaml`.

### Review expectations

krew-index maintainers check that:

- the plugin name is unique and matches the `kubectl-mitos` binary name
  convention (`kubectl <name>` invocation);
- every platform entry has a working `uri`, a correct `sha256`, and a `bin`
  that exists at the archive root (`kubectl-mitos`, or
  `kubectl-mitos.exe` on windows);
- `shortDescription`, `description`, and `homepage` are present and accurate;
- the manifest passes `kubectl krew install --manifest`.

Once the first krew-index PR merges, subsequent releases are handled by the
bot opening version-bump PRs automatically.
