# Console shell and website continuity: design

Status: draft for review. Date: 2026-06-25.

## Why

The console has a strong visual identity (the Fluorescence design system, shared
with the marketing site) but its information architecture is missing structural
patterns that world-class dashboards (Vercel, Linear, Stripe, Datadog, GitHub)
treat as table stakes: a global top bar, a consistent page-header, an
operational home, and power-user density. A user's first touchpoint is the
marketing site, then they progress into the app; that handoff must feel like one
continuous product, not two.

This spec defines the shell upgrade as a sequence of focused workstreams. It sits
above the feature surface in `2026-06-24-console-dashboard-enterprise-design.md`;
the per-project RBAC feature (B3d) is built on top of these patterns.

## What is already shared (the continuity we build on)

The console SPA and the Astro marketing site both consume `@mitos/brand` (the
Fluorescence design system):

- Same tokens: `--field #04050A`, `--magenta`, `--cyan`, `--ink`/`--ink-2`/`--ink-3`, `--hairline`.
- Same fonts: Satoshi (display/body), GeistMono (mono/data).
- Same button classes: `.btn`, `.btn-primary` (magenta + glow), `.btn-ghost`.

So continuity is not a rebuild. It is mirroring three things the site already
establishes, plus filling the structural gaps.

## Continuity rules (the site is the source of truth)

The marketing site nav (`website/src/layouts/Site.astro`) defines the chrome the
app must continue:

1. **Brand:** the real Mitos mark with a magenta drop-shadow glow, the capital
   "Mitos" wordmark, weight 600, letter-spacing -.02em. (Shipped in the brand
   continuity commit.)
2. **Top bar:** a 64px sticky bar, translucent field background
   (`rgba(4,5,10,.55)` + `blur(12px)`), hairline bottom border that appears on
   scroll.
3. **Page rhythm:** sub-pages use a `.page-hero` pattern: eyebrow + large title +
   lede + an actions row. The app's PageHeader mirrors this.

## Workstreams

Each workstream is roughly one PR, TDD, accessible (axe), responsive, no em/en
dashes, both golangci-lint invocations clean where Go is touched.

### 1. Brand continuity (DONE)

Shipped: the `Mark` component (filled disc dividing into an open ring) with an
optional magenta `glow`, used as the nav logo; capital "Mitos" wordmark in the
sidebar, top bar, page title, and residency badge; home renamed "Overview"; the
unreviewed Trust view removed. `Division` stays as the fork-tree motif.

### 2. Global top bar

A persistent 64px top bar across the app, styled to continue the site nav
(translucent blurred field, hairline-on-scroll). Layout:

- **Left:** the Mitos brand (mark + wordmark), linking to Overview. An **org
  switcher** chip (`acme v`) when `capabilities.orgSwitcher` is true; it lists
  the caller's memberships (from `/console/account`) and switches active org.
- **Center/right:** a **global search field** that opens the existing command
  palette (`Cmd-K`), with the shortcut hint visible so the palette is
  discoverable. The field is a button styled as an input; pressing it or `Cmd-K`
  opens the palette.
- **Right:** a **help** link (to docs) and an **account avatar menu** (the
  caller's initial) with: display name + email, a link to account settings, theme
  toggle, and sign out. Account settings moves here (see workstream 4).

The sidebar loses its inline brand block (the brand now lives in the top bar);
the sidebar becomes nav groups only, below the top bar. Mobile: the top bar keeps
the brand + the hamburger (which opens the existing nav drawer) + the search
trigger; the account menu collapses into the drawer.

Honesty: the org switcher only lists orgs the caller is a member of; switching
re-scopes every query (the BFF already enforces org from context, so a switch is
a client-side active-org change that changes the requests' effective org via the
session, never a way to see another org's data).

Components: `TopBar`, `OrgSwitcher`, `SearchTrigger`, `AccountMenu`. Data:
reuse `useCapabilities`, `useAccount`.

### 3. PageHeader component

One shared `PageHeader` adopted by every view, mirroring `.page-hero`:

- `eyebrow` (the nav group name, e.g. "GOVERN"), `title`, optional `lede`, and an
  optional right-aligned `actions` slot (primary action button).

Every view replaces its ad-hoc `<h2>` with `<PageHeader>`. This is what makes the
app feel uniform: same header zone, same place for the primary action (Sandboxes
gets "New sandbox", Members gets "Invite member", Projects gets "New project",
Secrets gets "Add secret", Keys gets "Create key"). Where the action is not yet
wired, the slot is omitted (no dead buttons).

### 4. Navigation regroup

The sidebar groups become:

- **Run:** Overview, Sandboxes, Fork tree
- **Build:** Workspaces, Templates, Secrets, API keys
- **Govern:** Members, Projects, Audit, Data and retention
- **Billing:** Usage, Billing (split out of Govern; financial, not governance)

Account "Settings" moves from a sidebar group into the account avatar menu
(workstream 2). The `Settings` group is removed from the sidebar. The
`GROUP_ORDER` becomes `Run, Build, Govern, Billing`. Routes for account settings
remain (reached from the avatar menu), just not as a left-nav group.

### 5. Overview redesign

Keep the measured-proof metrics as a **hero band** at the top (the honest-numbers
thesis), then add operational panels below so the home answers "what is happening
and what needs me":

- **Hero band:** Activate P50/P99, CoW savings as the headline strip (compact,
  not five equal cards). Every number stays reproducible (the "Reproduce this"
  affordance is retained).
- **Running now:** a compact count + list of running sandboxes (from
  `/console/sandboxes`), linking into Sandboxes.
- **Spend this month:** the current spend from `/console/billing` (when billing is
  enabled), linking into Billing.
- **Recent activity:** the last few audit events (from `/console/audit`), linking
  into Audit.

No new BFF endpoints; the panels compose existing queries. Empty/loading states
per panel. When `proof` is false the hero band shows the existing "no measured
signal yet" empty state but the operational panels still render (so the home is
never a dead end). This also lets us drop the `proof` gate on the home route so
the Overview is always present (best practice: the home never disappears).

### 6. Table toolbar and density

A shared `TableToolbar` (a filter control + an in-table search box + a result
count) and denser table rows with a row-actions affordance, applied to the list
views (Sandboxes, Members, Secrets, Templates, Keys, Projects). Power-user layer:
client-side filter/search over already-fetched rows; keyboard focus order; a
per-row actions menu. No BFF changes; purely a presentation upgrade over existing
data.

## Sequencing

`1 (done) -> 2 -> 3 -> 4` are the orientation bones; `5 -> 6` add depth; then
**B3d (per-project RBAC)** is built on top, so the Members/Projects/custom-role
views are born with PageHeader + TableToolbar.

## Out of scope here

- B3d feature logic (custom roles, per-project RBAC, project-tagged resources):
  its own spec and plans.
- Pricing/tiering and the hosted signup flow (the site's "Get started" target).
- Any change to BFF authorization semantics (org-from-context stays the law).

## Testing and quality floor

- TDD per workstream; Vitest + vitest-axe (zero violations) for new components.
- Responsive to mobile; visible keyboard focus; reduced-motion respected.
- TypeScript strict clean; `pnpm -C web/app test`, `typecheck`, `build` green.
- No em/en dashes anywhere.
- UX verified via the Playwright screenshot harness (desktop + mobile) per
  workstream, and via the local mock preview server for click-through QA.
