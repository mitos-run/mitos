# README rehaul: structure, quickstart, and hosted-docs front door

Date: 2026-06-27
Status: design (brainstorm output). Implementation plan is a follow-up.
Scope: two repos. `mitos-run/mitos` (the README) and `mitos-run/website` (one docs-hub back-link).

## Summary

The current `README.md` is 408 lines. It is accurate and thorough, but it does
not read as a world-class front door: the quickstart is split across two
overlapping code blocks that both demonstrate create plus fork, the lead does
not state plainly what Mitos is for a five-second newcomer, and a 20-row
documentation table duplicates an information architecture that the hosted docs
site already owns better.

This rehaul keeps everything readers value (the header, the six capability
tables, the architecture diagram, the comparison tables, the honesty hedges)
and fixes the structure: one non-duplicated quickstart ladder, a plain
"What is Mitos" lead, and a single hosted-docs pointer in place of the deep-link
table. Target length is roughly 260 lines (a moderate restructure, not a
landing-page rewrite).

It also closes the loop the user cares about: the README sends readers to the
hosted docs at `https://mitos.run/docs` (better rendered), and the hosted docs
hub gains a "Full docs in the repo" back-link so the complete `docs/` long tail
is one click away. The hosted site stays a curated allowlist; the repo `docs/`
remains the exhaustive source. Front door plus full long tail, no broken links.

## Guiding principles (from CLAUDE.md and the launch-journey spec)

- **Experience is DNA** (CLAUDE.md principle 6): no dead ends, simple surface
  with depth one click down, intent-shaped aha.
- **No unverified claims** (principle 1): every number stays reproducible from
  `bench/` or carries an issue reference. We keep the existing `~27 ms P50`
  framing and the comparison disclaimer verbatim. We do not add "sub-second
  boot" blanket copy or the refuted adoption stat (the two landmines named in
  the launch-journey spec).
- **Honest Kubernetes semantics** (principle 3): keep "Sandboxes are not pods"
  and the husk/raw `engine path` caveats. The trim never removes an honesty
  hedge; hedges move with the claim they qualify, they do not disappear.

## Decisions locked in this brainstorm

1. **Aggressiveness: moderate restructure.** Reorder for a quickstart-first
   flow and tighten prose; keep the capability tables, architecture diagram, and
   comparison tables in-README. Land near 260 lines (down from 408).
2. **Docs linking: hosted hub front door.** The README's Documentation section
   points at `https://mitos.run/docs` instead of carrying a 20-row deep-link
   table. Inline doc links that remain in feature/comparison tables link to the
   hosted page where it exists on the curated site and repo-relative
   (`docs/<x>.md`) otherwise, so nothing 404s. We do NOT expand the website
   allowlist and we do NOT mirror all docs; the curation policy in
   `website/scripts/lib/docs-manifest.mjs` is left intact.
3. **Website back-link.** The hosted docs hub (`website/src/pages/docs/index.astro`)
   gains a single, restrained link back to the repo `docs/` directory
   (`https://github.com/mitos-run/mitos/tree/main/docs`) so the curated site is
   the front door and the full `docs/` tree is reachable in one click.
4. **Two repos, two PRs.** README change in `mitos`; back-link change in
   `website`. Independent, no ordering dependency.

## Target README structure

The header block (lines 1-40 today: logo picture, tagline, badges, nav row,
`demo.gif`) is kept as-is; the user explicitly likes it. The one nav-row edit:
the "Documentation" entry points to `https://mitos.run/docs` rather than the
relative `docs/` path.

| # | Section | Action | Notes |
|---|---|---|---|
| 1 | Hero (logo, tagline, badges, nav, demo.gif) | KEEP | Only nav "Documentation" link retargets to the hosted hub. |
| 2 | What is Mitos | NEW | 2-3 plain sentences: what it is, who it is for, the one-line aha ("fork a running microVM into parallel agent attempts"). Newcomer gets it in five seconds. Mirrors the homepage hero framing without inventing claims. |
| 3 | Quickstart | REWRITE | One ladder, no duplication (see below). |
| 4 | Why Mitos | TIGHTEN | Keep the three differentiator bullets (live-fork, ~27 ms warm-claim, open-source/self-hostable/k8s-native), the "two ways to run" pair, and the husk/raw `engine path` default note. Trim prose. |
| 5 | Features | KEEP | The six capability tables (Speed, Isolation, Agent DX, Kubernetes-native, Durable state, Operable). Inline links: hosted-where-exists, repo-relative otherwise. |
| 6 | Architecture | KEEP | Mermaid diagram, claim/exec path bullets, "Sandboxes are not pods" paragraph. |
| 7 | Comparison | KEEP | Both tables and the "other vendors' published numbers, not a head-to-head" disclaimer, verbatim. |
| 8 | Project status | TRIM | Keep the pre-1.0 honesty, the "verified on a real KVM cluster" list, and the "tracked tails" list; tighten wording. |
| 9 | Documentation | COLLAPSE | Replace the ~20-row table with a short pointer: hosted docs at `mitos.run/docs` for the curated set; the full `docs/` tree in the repo for everything else. |
| 10 | Local development | KEEP or fold | Keep as a short section; it is useful for contributors. May move below Documentation. |
| 11 | Contributing / Security / License | KEEP | Unchanged. |

### The quickstart fix (the core of this work)

Today two blocks both show create plus fork: "Try it in a few lines" (lines
42-63) and "Quickstart > Python" (lines 80-104). The rehaul merges them into one
ladder that each step builds on, with no repeated create-then-fork:

- **a) Install and key.** `pip install mitos-run`; `export MITOS_API_KEY=...`
  (a hosted key from `https://mitos.run`, no Kubernetes required); note that the
  same code targets your own cluster via `MITOS_BASE_URL`.
- **b) First sandbox.** `mitos.create("python")` then `exec` (the current
  "Try it" snippet, now the single canonical first contact).
- **c) Fork into siblings.** The `sb.fork(2)` example, shown once, here.
- **d) Run it your way.** A compact pointer block: other languages (the existing
  six-row SDK table), cluster mode (`AgentRun`), the CLI, and integrations. These
  keep their existing tables/snippets but sit under one "your way" umbrella
  instead of as peer top-level quickstart subsections, so the primary path is
  unmistakable and the alternatives are depth one click down.

Files, `run_code`, streaming exec, and the "Beyond exec" snippet stay, but as a
short "more on the handle" note rather than a second full walkthrough. The
`run_code` / streaming / PTY engine-path caveats are preserved.

## Website back-link

`website/src/pages/docs/index.astro` renders the docs hub (`<h1>Documentation</h1>`
plus a lede and grouped cards). Add one restrained element below the lede (or in
the shared footer if more appropriate on inspection): a link reading roughly
"Looking for a deeper reference? The full docs tree lives in the repo." pointing
to `https://github.com/mitos-run/mitos/tree/main/docs`. Styling follows the
existing brand tokens already in that file (`--ink-2`, `--magenta`, hairline
borders). No new dependency, no allowlist change, no sync-script change.

## Out of scope

- Expanding the website docs allowlist or mirroring all of `docs/` (explicitly
  rejected in favor of the back-link).
- Any new benchmark, claim, or number. Copy reuses existing verified figures.
- Restructuring the docs site navigation or adding new doc pages.
- Touching the SDKs, CLI, or any code path. Documentation only.

## Verification

- **No broken links.** Script-check every link in the new README: hosted
  `mitos.run/docs/*` links resolve 200; repo-relative `docs/*.md` links point at
  files that exist; anchor links resolve to real headings.
- **Punctuation.** Zero em (U+2014) or en (U+2013) dashes in the README and the
  website change (CLAUDE.md strict rule). Grep both diffs.
- **Length.** README lands in the ~240-280 line band.
- **Honesty preserved.** Confirm every claim that carried a hedge or issue
  reference today still carries it: `~27 ms P50` reproducibility pointer, the
  comparison disclaimer, "Sandboxes are not pods", the husk/raw `engine path`
  notes, the `KernelUnavailable` fail-closed note, and the pre-1.0 status.
- **Website build.** `npm run build` (or the repo's check-docs-build script) in
  `website` succeeds with the back-link added.
- **Render check.** README renders correctly on GitHub (the picture/srcset
  header, the mermaid block, the tables).
```
