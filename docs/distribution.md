# Distribution and registries

How the mitos artifacts reach public registries, and what is automated versus
what is a one-time manual or account step. Every listing is configured to
backlink to https://mitos.run.

Four channels:

| Channel | What ships | Source of truth | Per-channel runbook |
| --- | --- | --- | --- |
| Artifact Hub | the Helm chart | `deploy/charts/mitos` | this file |
| krew (kubectl plugins) | the `kubectl mitos` plugin | `.krew.yaml`, `cmd/kubectl-mitos` | [krew.md](krew.md) |
| OperatorHub.io | the OLM operator bundle | `deploy/olm/bundle` | [operatorhub.md](operatorhub.md) |
| Red Hat Certified Operators | the same OLM bundle, certified | `deploy/olm/bundle` | [redhat-certification.md](redhat-certification.md) |

## Backlink summary

Every external listing points at the canonical domain so the links compound:

| Listing field | Value |
| --- | --- |
| Chart `home`, maintainer url, Artifact Hub links | https://mitos.run |
| krew plugin `homepage` | https://mitos.run |
| CSV `provider.url`, `links[Website]` | https://mitos.run |
| Helm repo URL (what users `helm repo add`) | https://mitos.run/charts |

## Artifact Hub (Helm chart)

### What is automated

`.github/workflows/helm-release.yaml` runs on changes under `deploy/charts/**`.
It packages the chart with `helm/chart-releaser-action`, creates a GitHub
release with the `.tgz`, writes `index.yaml` to the `gh-pages` branch, and copies
`deploy/charts/artifacthub-repo.yml` to the `gh-pages` root.

chart-releaser only publishes a chart version that has no release yet, so bump
`deploy/charts/mitos/Chart.yaml` `version` for every chart change. It is at
`0.2.0` now.

### One-time setup

1. Create an empty `gh-pages` branch (chart-releaser requires it to exist):

   ```
   git switch --orphan gh-pages
   git commit --allow-empty -m "chore: seed gh-pages"
   git push origin gh-pages
   git switch main
   ```

2. Enable GitHub Pages for the repo: Settings, Pages, Source = `gh-pages`
   branch, root. This serves the repo at `https://mitos-run.github.io/mitos/`.

3. Serve it from `https://mitos.run/charts` (see the next section).

4. Register the repository on Artifact Hub: add a repository of kind Helm with
   url `https://mitos.run/charts`. Copy the generated repository ID into
   `deploy/charts/artifacthub-repo.yml` (`repositoryID`), re-run the release
   workflow so the file ships with the ID, then claim ownership on Artifact Hub.

### Serving the repo at https://mitos.run/charts (website and Cloudflare)

Yes, this needs one change outside the engine repo, because `mitos.run` is served
by the website (the separate Astro/Starlight site) and GitHub Pages publishes the
chart index under the project path `https://mitos-run.github.io/mitos/`. Three
ways to bridge them, pick one:

- Option A, Cloudflare proxy to the exact path (recommended, matches the wired
  URL). Add a Cloudflare Worker on the route `mitos.run/charts/*` that fetches
  the same path from the Pages origin and returns it, rewriting the prefix:
  `mitos.run/charts/<x>` to `https://mitos-run.github.io/mitos/<x>`. This keeps
  the `https://mitos.run/charts` URL on the response (a true rewrite, not a
  redirect), which is the cleanest for the backlink. Only `index.yaml` and
  `artifacthub-repo.yml` are fetched from this origin; the chart `.tgz` URLs in
  `index.yaml` point at GitHub release assets and resolve on their own.

- Option B, subdomain via GitHub Pages custom domain (simplest, no Worker). Add a
  DNS CNAME `charts.mitos.run` to `mitos-run.github.io` in Cloudflare, set it as
  the Pages custom domain (writes a `CNAME` file on `gh-pages`), and use
  `https://charts.mitos.run` as the repo URL instead. Still a `mitos.run`
  backlink. If you take this option, change the repo URL in the chart README,
  `deploy/charts/mitos/Chart.yaml`, and the Artifact Hub registration to
  `https://charts.mitos.run`.

- Option C, publish into the website repo (no Cloudflare rule, true same-origin).
  Extend the release workflow to push `index.yaml`, the `.tgz`, and
  `artifacthub-repo.yml` into the website repo `static/charts/` with a deploy
  key. More moving parts and cross-repo creds, but the chart is then served
  natively from `mitos.run` with no proxy.

A plain Cloudflare redirect rule (301/302) also works for `helm` and Artifact Hub
because both follow redirects, but the fetched URL becomes the `github.io` one,
which is why Option A uses a rewrite instead.

### Optional: signed charts (Artifact Hub Signed badge)

The classic repo path shows the Signed badge when `.prov` provenance files are
published and `artifacthub.io/signKey` names the public key. That needs a
maintainer-held GPG key (`helm package --sign`), so it is deferred until a key is
provisioned. The container images are already cosign keyless signed and SBOM
attested in `.github/workflows/publish.yaml`; the `artifacthub.io/images`
annotation lists them so Artifact Hub scans them.

## krew, OperatorHub.io, Red Hat

See the per-channel runbooks linked in the table above. The OLM bundle under
`deploy/olm/bundle` is shared by both OperatorHub.io and the Red Hat certified
path. Note the technical fit caveat for the Red Hat path: mitos needs KVM and
nested virtualization plus a privileged DaemonSet, which OpenShift restricts; the
certified path is gated on real OpenShift-on-bare-metal demand. Details in
[redhat-certification.md](redhat-certification.md).
