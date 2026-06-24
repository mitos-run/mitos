# Console B1: hero views (instrument cockpit + live fork tree) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the two signature views of the console: the Instruments cockpit (the org's own measured activate latency and CoW density, with a "Reproduce this" affordance) and the live fork tree (the `Division` motif rendered as an interactive copy-on-write tree, the one view no competitor can draw).

**Architecture:** Frontend React views on the B0 shell (TanStack Query data layer, routes config, AppShell, primitives), reading two BFF surfaces. The cockpit reads the existing `/console/instruments`. The fork tree reads a new org-scoped `ForkTreeSource` seam plus `/console/forktree` endpoint, added in Go and mirroring the existing `SandboxControl` seam (interface + in-memory fake now; the real controller-backed implementation that walks husk-pod CoW lineage is a documented follow-up). Layout math is a pure, unit-tested function separate from rendering. Both views are fully responsive and accessible (WCAG 2.2 AA), per spec section 4.6, including a screen-reader-accessible data-table alternative to the SVG visualization.

**Tech Stack:** Go (BFF seam, `net/http`, `encoding/json`), React 18 + Vite + TypeScript strict, TanStack Query, `@mitos/brand` (Fluorescence tokens, the `Division` SVG motif), Vitest + Testing Library + vitest-axe, SVG for the fork-tree rendering (no new charting dependency).

**Scope note:** Second of four plans from `docs/superpowers/specs/2026-06-24-console-dashboard-enterprise-design.md` (sections 4.4, 7, 9). B0 (shell) is merged in PR #332. This is B1. B2 (core views + projects + profile) and B3 (enterprise layer) follow. The per-sandbox fork-tree detail tab is B2 (Sandboxes detail tabs); B1 ships the standalone fork-tree hero view.

## Global Constraints

Every task implicitly includes these. Values copied verbatim from the spec and CLAUDE.md.

- **Punctuation (strict):** never use em (U+2014) or en (U+2013) dashes anywhere (Go, TS/TSX, comments, JSX/Go-string copy, Markdown, commit messages). Use only `.` `,` `;` `:` connectors; ASCII hyphen-minus `-` for ranges/compounds. Verify before commit: `grep -rnP '\xe2\x80\x94|\xe2\x80\x93'` on changed files is empty.
- **Commits:** conventional commits; DCO sign-off on every commit (`git commit -s`).
- **Git staging:** explicit paths only; never `git add -A`. The pnpm lockfile is at `web/pnpm-lock.yaml` (workspace root).
- **No unverified numbers (integrity rule):** the cockpit shows ONLY the org's own measured numbers from `/console/instruments`. No fabricated or hard-coded metric values in UI source. Any competitor figure, if ever shown, carries the vendor-published / not-head-to-head label verbatim. The "Reproduce this" affordance points at the in-repo bench (`bench/husk-activate-latency.sh`, `cmd/bench`).
- **Org-scoped isolation (BFF):** the new `/console/forktree` endpoint reads the caller's org from context (never from query/path/body) and returns only that org's data; a cross-org id resolves to `not_found`. Every new endpoint gets a cross-org isolation test, matching the existing console BFF contract.
- **Secrets rule:** no secret value is logged, returned, or embedded; the fork-tree and instruments payloads carry no secret.
- **Responsive + accessible (spec 4.6), every view:** both views work first-class on phone/tablet/desktop; keyboard-operable; the SVG fork tree has a screen-reader-accessible data-table alternative; AA contrast (token-driven); `prefers-reduced-motion` honored; an axe-core test asserts zero violations.
- **Go conventions:** `fmt.Errorf("context: %w", err)`; octal `0o644`; gofmt + golangci-lint clean (run BOTH `golangci-lint run --timeout=5m` and `GOOS=linux golangci-lint run --timeout=5m`); production code is not excluded from errcheck.
- **TypeScript strict** stays clean (`pnpm -C web/app typecheck`); the SPA unit suite stays green and exits 0 (`pnpm -C web/app test`).

---

## File Structure

Created or modified:

- `web/app/src/data/instruments.ts` (create) - `useInstruments()` TanStack Query hook over the existing `api.instruments()`.
- `web/app/src/ui/StatTile.tsx` (create) - the instrument tile primitive (label, value, unit, optional trend, "Reproduce this" popover).
- `web/app/src/ui/StatTile.test.tsx` (create).
- `web/app/src/views/Instruments.tsx` (modify) - replace the B0 stub with the real cockpit.
- `web/app/src/views/Instruments.test.tsx` (create).
- `internal/saas/console/forktree.go` (create) - `ForkNode`, `ForkTree`, the `ForkTreeSource` seam, an in-memory fake, and the `GET /console/forktree` handler.
- `internal/saas/console/forktree_test.go` (create) - handler + cross-org isolation tests.
- `internal/saas/console/console.go` (modify) - register the route and the `ForkTree` dep (with an in-memory default), following the existing seam-default pattern.
- `web/app/src/data/forktree.ts` (create) - typed `ForkTree`/`ForkNode`, `api.forktree()`, `useForkTree()`, and a dev fixture.
- `web/app/src/views/forktree/layout.ts` (create) - pure layout function (nodes -> positioned nodes + edges).
- `web/app/src/views/forktree/layout.test.ts` (create).
- `web/app/src/views/forktree/ForkTree.tsx` (create) - the SVG visualization + accessible data-table alternative.
- `web/app/src/views/forktree/ForkTree.test.tsx` (create).
- `web/app/src/views/forktree/ForkTree.a11y.test.tsx` (create).
- `web/app/src/nav/routes.tsx` (modify) - add the `/forks` route (group Run), rendering the fork-tree view.
- `web/packages/brand/src/base.css` (modify) - cockpit grid and fork-tree node styles (token-driven).

---

### Task 1: Instruments data hook

**Files:**
- Create: `web/app/src/data/instruments.ts`
- Test: `web/app/src/data/instruments.test.tsx`

**Interfaces:**
- Consumes: `api.instruments()` and the `Instruments` type from `web/app/src/api.ts` (already present: `activate_p50_ms`, `activate_p99_ms`, `forks_served`, `cow_savings_bytes`, `marginal_bytes_per_fork`).
- Produces: `useInstruments(): UseQueryResult<Instruments>`.

- [ ] **Step 1: Write the failing test `web/app/src/data/instruments.test.tsx`**

```tsx
import { describe, it, expect, vi } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { useInstruments } from './instruments'

function wrapper() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return ({ children }: { children: React.ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  )
}

describe('useInstruments', () => {
  it('fetches the org instruments document', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ org_id: 'o1', activate_p50_ms: 27, activate_p99_ms: 41, forks_served: 10, cow_savings_bytes: 2304, marginal_bytes_per_fork: 3 }), {
        status: 200, headers: { 'content-type': 'application/json' },
      }),
    )
    const { result } = renderHook(() => useInstruments(), { wrapper: wrapper() })
    await waitFor(() => expect(result.current.data).toBeDefined())
    expect(result.current.data?.activate_p50_ms).toBe(27)
  })
})
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `pnpm -C web/app test src/data/instruments.test.tsx`
Expected: FAIL with "Cannot find module './instruments'".

- [ ] **Step 3: Implement `web/app/src/data/instruments.ts`**

```ts
// The instrument cockpit's data source: the org's own measured activate latency
// and CoW density from the #211/#33 pipeline. Polls at a slow cadence so the
// cockpit stays live without hammering the BFF.
import { useQuery } from '@tanstack/react-query'
import { api, type Instruments } from '../api'

export function useInstruments() {
  return useQuery<Instruments>({
    queryKey: ['instruments'],
    queryFn: () => api.instruments(),
    staleTime: 15_000,
    refetchInterval: 30_000,
  })
}
```

- [ ] **Step 4: Run it and confirm it passes**

Run: `pnpm -C web/app test src/data/instruments.test.tsx`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/app/src/data/instruments.ts web/app/src/data/instruments.test.tsx
git commit -s -m "feat(console): add useInstruments hook for the cockpit"
```

---

### Task 2: StatTile primitive

**Files:**
- Create: `web/app/src/ui/StatTile.tsx`
- Test: `web/app/src/ui/StatTile.test.tsx`

**Interfaces:**
- Consumes: `@mitos/brand` `Card`.
- Produces: `StatTile({ label, value, unit?, hint?, reproduce? })` where `reproduce?: { label: string; command: string }` renders a "Reproduce this" disclosure showing the in-repo bench command.

- [ ] **Step 1: Write the failing test `web/app/src/ui/StatTile.test.tsx`**

```tsx
import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { StatTile } from './StatTile'

describe('StatTile', () => {
  it('renders label, value, and unit', () => {
    render(<StatTile label="Activate P50" value="27" unit="ms" />)
    expect(screen.getByText('Activate P50')).toBeInTheDocument()
    expect(screen.getByText('27')).toBeInTheDocument()
    expect(screen.getByText('ms')).toBeInTheDocument()
  })

  it('discloses the reproduce command on demand', async () => {
    render(<StatTile label="Activate P50" value="27" unit="ms" reproduce={{ label: 'Reproduce this', command: 'bench/husk-activate-latency.sh' }} />)
    const btn = screen.getByRole('button', { name: /reproduce this/i })
    expect(screen.queryByText('bench/husk-activate-latency.sh')).not.toBeInTheDocument()
    await userEvent.click(btn)
    expect(screen.getByText('bench/husk-activate-latency.sh')).toBeInTheDocument()
    expect(btn).toHaveAttribute('aria-expanded', 'true')
  })
})
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `pnpm -C web/app test src/ui/StatTile.test.tsx`
Expected: FAIL with "Cannot find module './StatTile'".

- [ ] **Step 3: Implement `web/app/src/ui/StatTile.tsx`**

```tsx
// The instrument tile: one measured number with its label and unit, and an
// optional "Reproduce this" disclosure that names the in-repo bench command.
// Integrity as a feature: every headline metric can be reproduced. Value strings
// are formatted by the caller from real BFF data; this primitive never invents a
// number.
import { useState } from 'react'
import { Card } from '@mitos/brand'

export type StatTileProps = {
  label: string
  value: string
  unit?: string
  hint?: string
  reproduce?: { label: string; command: string }
}

export function StatTile({ label, value, unit, hint, reproduce }: StatTileProps) {
  const [open, setOpen] = useState(false)
  return (
    <Card style={{ padding: 'var(--space-5)' }}>
      <div className="t-dim" style={{ fontSize: 'var(--step--1)', textTransform: 'uppercase', letterSpacing: '0.08em' }}>{label}</div>
      <div style={{ display: 'flex', alignItems: 'baseline', gap: 'var(--space-2)', marginTop: 'var(--space-2)' }}>
        <span style={{ fontSize: 'var(--step-4)', fontFamily: 'var(--mono)', color: 'var(--cyan)' }}>{value}</span>
        {unit && <span className="t-dim">{unit}</span>}
      </div>
      {hint && <div className="t-dim" style={{ fontSize: 'var(--step--1)', marginTop: 'var(--space-2)' }}>{hint}</div>}
      {reproduce && (
        <div style={{ marginTop: 'var(--space-3)' }}>
          <button
            className="btn btn-ghost"
            aria-expanded={open}
            onClick={() => setOpen((v) => !v)}
            style={{ fontSize: 'var(--step--1)' }}
          >
            {reproduce.label}
          </button>
          {open && (
            <pre className="t-dim" style={{ marginTop: 'var(--space-2)', fontSize: 'var(--step--1)', overflowX: 'auto' }}>
              <code>{reproduce.command}</code>
            </pre>
          )}
        </div>
      )}
    </Card>
  )
}
```

- [ ] **Step 4: Run it and confirm it passes**

Run: `pnpm -C web/app test src/ui/StatTile.test.tsx`
Expected: PASS, 2 tests.

- [ ] **Step 5: Commit**

```bash
git add web/app/src/ui/StatTile.tsx web/app/src/ui/StatTile.test.tsx
git commit -s -m "feat(console): add StatTile primitive with reproduce-this disclosure"
```

---

### Task 3: Instruments cockpit view

**Files:**
- Modify: `web/app/src/views/Instruments.tsx`
- Test: `web/app/src/views/Instruments.test.tsx`

**Interfaces:**
- Consumes: `useInstruments()` (Task 1), `StatTile` (Task 2), `Skeleton` and `EmptyState` (B0), `fmtBytes` from `api.ts`.
- Produces: the cockpit view (default export style matching the existing `Instruments` export name).

- [ ] **Step 1: Write the failing test `web/app/src/views/Instruments.test.tsx`**

```tsx
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { Instruments } from './Instruments'

function wrap(ui: React.ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>)
}

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockResolvedValue(
    new Response(JSON.stringify({ org_id: 'o1', activate_p50_ms: 27, activate_p99_ms: 41, forks_served: 10, cow_savings_bytes: 2415919104, marginal_bytes_per_fork: 3145728 }), {
      status: 200, headers: { 'content-type': 'application/json' },
    }),
  )
})

describe('Instruments cockpit', () => {
  it('renders the measured activate latency and CoW density tiles', async () => {
    wrap(<Instruments />)
    await waitFor(() => expect(screen.getByText('27')).toBeInTheDocument())
    expect(screen.getByText(/Activate P50/i)).toBeInTheDocument()
    expect(screen.getByText(/Activate P99/i)).toBeInTheDocument()
    expect(screen.getByText(/Forks served/i)).toBeInTheDocument()
    expect(screen.getByText('10')).toBeInTheDocument()
  })
})
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `pnpm -C web/app test src/views/Instruments.test.tsx`
Expected: FAIL (the B0 stub renders different content; the assertions for the tiles fail).

- [ ] **Step 3: Implement the cockpit in `web/app/src/views/Instruments.tsx`**

```tsx
// The cockpit: the org's OWN measured numbers, not a welcome screen. Activate
// latency (warm-claim P50/P99, their cluster), CoW density (memory saved by
// page-sharing) and marginal bytes per fork, forks served. Every headline metric
// carries a "Reproduce this" affordance pointing at the in-repo bench. No number
// is invented here; all values come from /console/instruments.
import { useInstruments } from '../data/instruments'
import { StatTile } from '../ui/StatTile'
import { Skeleton } from '../ui/Skeleton'
import { EmptyState } from '../ui/EmptyState'
import { fmtBytes } from '../api'

const BENCH = 'bench/husk-activate-latency.sh'

export function Instruments() {
  const { data, isLoading, error } = useInstruments()
  if (error) return <EmptyState title="Instruments unavailable" body="The telemetry pipeline could not be read for this organization." />
  if (isLoading || !data) return <Skeleton rows={4} />

  const noData = data.forks_served === 0 && data.activate_p50_ms === 0
  if (noData) {
    return (
      <EmptyState
        title="No measured signal yet"
        body="Fork a sandbox to see this org's activate latency and CoW density here. Only measured signal emits light."
      />
    )
  }

  return (
    <section>
      <h2>Instruments</h2>
      <p className="t-dim">This organization's measured signal. Every number is reproducible.</p>
      <div className="cockpit-grid">
        <StatTile label="Activate P50" value={String(Math.round(data.activate_p50_ms))} unit="ms" hint="warm-claim, your cluster" reproduce={{ label: 'Reproduce this', command: BENCH }} />
        <StatTile label="Activate P99" value={String(Math.round(data.activate_p99_ms))} unit="ms" hint="warm-claim, your cluster" reproduce={{ label: 'Reproduce this', command: BENCH }} />
        <StatTile label="CoW savings" value={fmtBytes(data.cow_savings_bytes)} hint="memory not spent, forks share parent pages" reproduce={{ label: 'Reproduce this', command: BENCH }} />
        <StatTile label="Marginal / fork" value={fmtBytes(data.marginal_bytes_per_fork)} hint="mean private-dirty set per fork" reproduce={{ label: 'Reproduce this', command: BENCH }} />
        <StatTile label="Forks served" value={String(data.forks_served)} hint="total for this org" />
      </div>
    </section>
  )
}
```

- [ ] **Step 4: Run it and confirm it passes**

Run: `pnpm -C web/app test src/views/Instruments.test.tsx`
Expected: PASS.

- [ ] **Step 5: Add the cockpit grid style to `web/packages/brand/src/base.css`**

Append (token-driven, responsive: auto-fit columns that reflow on narrow screens):

```css
/* Instrument cockpit: responsive tile grid. */
.cockpit-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(min(220px, 100%), 1fr));
  gap: var(--space-4);
  margin-top: var(--space-5);
}
```

- [ ] **Step 6: Run the full SPA suite and typecheck**

Run: `pnpm -C web/app test && pnpm -C web/app typecheck`
Expected: all pass, clean.

- [ ] **Step 7: Commit**

```bash
git add web/app/src/views/Instruments.tsx web/app/src/views/Instruments.test.tsx web/packages/brand/src/base.css
git commit -s -m "feat(console): build the instrument cockpit on measured org telemetry"
```

---

### Task 4: ForkTree BFF seam and endpoint (Go)

**Files:**
- Create: `internal/saas/console/forktree.go`
- Modify: `internal/saas/console/console.go` (register route + dep default)
- Test: `internal/saas/console/forktree_test.go`

**Interfaces:**
- Consumes: the console's `OrgFromContext` / `caller` org-scoping (existing); `apierr` for the error envelope; `ErrNotFound` (existing in this package).
- Produces:
  - `type ForkNode struct { ID, ParentID, Phase string; PrivateDirtyBytes, SharedBytes int64; CreatedAt time.Time }` with JSON tags.
  - `type ForkTree struct { OrgID string; Nodes []ForkNode }`.
  - `type ForkTreeSource interface { Tree(ctx context.Context, orgID string) (ForkTree, error) }`.
  - `NewMemForkTree()` in-memory fake.
  - `GET /console/forktree` handler on `Console`.
  - `Deps.ForkTree ForkTreeSource` (nil defaults to the in-memory fake in `New`).

- [ ] **Step 1: Write the failing test `internal/saas/console/forktree_test.go`**

```go
package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestForkTreeIsOrgScoped(t *testing.T) {
	mem := NewMemForkTree()
	mem.Set("orgA", []ForkNode{{ID: "s1", ParentID: "", Phase: "Running", PrivateDirtyBytes: 3 << 20, SharedBytes: 200 << 20}})
	mem.Set("orgB", []ForkNode{{ID: "s9", ParentID: "", Phase: "Running"}})
	c := New(Deps{ForkTree: mem})

	// orgA sees only its own node.
	req := httptest.NewRequest("GET", "/console/forktree", nil)
	req = req.WithContext(WithCaller(context.Background(), "acct", "orgA"))
	rr := httptest.NewRecorder()
	c.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got ForkTree
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Nodes) != 1 || got.Nodes[0].ID != "s1" {
		t.Fatalf("orgA tree = %+v, want exactly s1", got.Nodes)
	}
	// orgB's node must never appear in orgA's response.
	for _, n := range got.Nodes {
		if n.ID == "s9" {
			t.Fatalf("orgB node s9 leaked into orgA forktree")
		}
	}
}

func TestForkTreeRequiresOrgContext(t *testing.T) {
	c := New(Deps{ForkTree: NewMemForkTree()})
	req := httptest.NewRequest("GET", "/console/forktree", nil)
	rr := httptest.NewRecorder()
	c.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no org context)", rr.Code)
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/saas/console/ -run TestForkTree`
Expected: FAIL to compile ("undefined: NewMemForkTree", "undefined: ForkNode").

- [ ] **Step 3: Implement `internal/saas/console/forktree.go`**

```go
package console

import (
	"context"
	"sync"
	"time"
)

// ForkNode is one node in an org's live fork tree: a sandbox and its CoW
// relationship to its parent snapshot. PrivateDirtyBytes is the node's unique
// (copied) page set; SharedBytes is the memory it shares with its parent via
// copy-on-write. A node with an empty ParentID is a root snapshot. It carries no
// secret.
type ForkNode struct {
	ID                string    `json:"id"`
	ParentID          string    `json:"parent_id"`
	Phase             string    `json:"phase"`
	PrivateDirtyBytes int64     `json:"private_dirty_bytes"`
	SharedBytes       int64     `json:"shared_bytes"`
	CreatedAt         time.Time `json:"created_at"`
}

// ForkTree is an org's fork forest: the nodes the console renders as the live
// CoW tree. The edges are implied by ParentID.
type ForkTree struct {
	OrgID string     `json:"org_id"`
	Nodes []ForkNode `json:"nodes"`
}

// ForkTreeSource is the org-scoped seam the fork-tree view reads. The REAL
// implementation walks the controller's claim records and the #33 CoW-aware
// metering to fill PrivateDirtyBytes / SharedBytes per node; this slice ships an
// injectable interface and an in-memory fake so the BFF shapes and org-scopes the
// tree now, and the cluster query is a documented follow-up. Tree MUST return
// only the named org's nodes.
type ForkTreeSource interface {
	Tree(ctx context.Context, orgID string) (ForkTree, error)
}

// MemForkTree is the in-memory tested default. It stores per-org node sets and
// never returns one org's nodes to another.
type MemForkTree struct {
	mu    sync.RWMutex
	byOrg map[string][]ForkNode
}

// NewMemForkTree returns an empty in-memory fork-tree source.
func NewMemForkTree() *MemForkTree {
	return &MemForkTree{byOrg: map[string][]ForkNode{}}
}

// Set replaces the node set for one org (test/seed helper).
func (m *MemForkTree) Set(orgID string, nodes []ForkNode) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byOrg[orgID] = nodes
}

// Tree returns only the named org's nodes.
func (m *MemForkTree) Tree(_ context.Context, orgID string) (ForkTree, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	nodes := m.byOrg[orgID]
	out := make([]ForkNode, len(nodes))
	copy(out, nodes)
	return ForkTree{OrgID: orgID, Nodes: out}, nil
}
```

- [ ] **Step 4: Wire the handler and dep in `internal/saas/console/console.go`**

Add `ForkTree ForkTreeSource` to the `Deps` struct (near `Instruments`), default it in `New` (after the `Instruments` default):

```go
	if deps.ForkTree == nil {
		deps.ForkTree = NewMemForkTree()
	}
```

Register the route in `routes()` (next to the instruments route):

```go
	mux.HandleFunc("GET /console/forktree", c.handleForkTree)
```

Add the handler (next to `handleInstruments`):

```go
func (c *Console) handleForkTree(w http.ResponseWriter, r *http.Request) {
	_, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	tree, err := c.deps.ForkTree.Tree(r.Context(), orgID)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the fork tree could not be read"))
		return
	}
	if tree.Nodes == nil {
		tree.Nodes = []ForkNode{}
	}
	writeJSON(w, http.StatusOK, tree)
}
```

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/saas/console/ -run TestForkTree`
Expected: PASS, 2 tests.

- [ ] **Step 6: Lint (both invocations) and full package test**

Run: `golangci-lint run --timeout=5m ./internal/saas/console/... && GOOS=linux golangci-lint run --timeout=5m ./internal/saas/console/...`
Run: `go test ./internal/saas/console/`
Expected: lint clean both ways; package tests pass.

- [ ] **Step 7: Commit**

```bash
git add internal/saas/console/forktree.go internal/saas/console/forktree_test.go internal/saas/console/console.go
git commit -s -m "feat(console): add org-scoped ForkTreeSource seam and /console/forktree"
```

---

### Task 5: Fork-tree data layer (client)

**Files:**
- Modify: `web/app/src/api.ts` (add the `ForkTree`/`ForkNode` types + `api.forktree()`)
- Create: `web/app/src/data/forktree.ts` (the `useForkTree` hook + a dev fixture)
- Test: `web/app/src/data/forktree.test.tsx`

**Interfaces:**
- Produces:
  - `export type ForkNode = { id: string; parent_id: string; phase: string; private_dirty_bytes: number; shared_bytes: number }`.
  - `export type ForkTree = { org_id: string; nodes: ForkNode[] }`.
  - `api.forktree(): Promise<ForkTree>`.
  - `useForkTree(): UseQueryResult<ForkTree>`.
  - `FORK_TREE_FIXTURE: ForkTree` (a small deterministic fixture for dev and stories).

- [ ] **Step 1: Write the failing test `web/app/src/data/forktree.test.tsx`**

```tsx
import { describe, it, expect, vi } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { useForkTree } from './forktree'

function wrapper() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return ({ children }: { children: React.ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  )
}

describe('useForkTree', () => {
  it('fetches the org fork tree', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ org_id: 'o1', nodes: [{ id: 's1', parent_id: '', phase: 'Running', private_dirty_bytes: 3, shared_bytes: 200 }] }), {
        status: 200, headers: { 'content-type': 'application/json' },
      }),
    )
    const { result } = renderHook(() => useForkTree(), { wrapper: wrapper() })
    await waitFor(() => expect(result.current.data).toBeDefined())
    expect(result.current.data?.nodes[0].id).toBe('s1')
  })
})
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `pnpm -C web/app test src/data/forktree.test.tsx`
Expected: FAIL with "Cannot find module './forktree'".

- [ ] **Step 3: Add the types and client method to `web/app/src/api.ts`**

Add near the other types:

```ts
export type ForkNode = {
  id: string
  parent_id: string
  phase: string
  private_dirty_bytes: number
  shared_bytes: number
}

export type ForkTree = { org_id: string; nodes: ForkNode[] }
```

Add to the `api` object:

```ts
  forktree: () => get<ForkTree>('/console/forktree'),
```

- [ ] **Step 4: Implement `web/app/src/data/forktree.ts`**

```ts
// The fork-tree view's data source plus a deterministic dev fixture. The fixture
// is used for local dev and the visualization tests; production reads the live
// /console/forktree endpoint.
import { useQuery } from '@tanstack/react-query'
import { api, type ForkTree } from '../api'

export function useForkTree() {
  return useQuery<ForkTree>({
    queryKey: ['forktree'],
    queryFn: () => api.forktree(),
    staleTime: 10_000,
    refetchInterval: 20_000,
  })
}

// A small, deterministic fork forest: one root snapshot with three forks, one of
// which forks again. Used by dev and the layout/visualization tests so they do
// not depend on a live cluster.
export const FORK_TREE_FIXTURE: ForkTree = {
  org_id: 'fixture',
  nodes: [
    { id: 'root', parent_id: '', phase: 'Running', private_dirty_bytes: 0, shared_bytes: 209715200 },
    { id: 'fork-a', parent_id: 'root', phase: 'Running', private_dirty_bytes: 3145728, shared_bytes: 209715200 },
    { id: 'fork-b', parent_id: 'root', phase: 'Running', private_dirty_bytes: 4194304, shared_bytes: 209715200 },
    { id: 'fork-c', parent_id: 'root', phase: 'Running', private_dirty_bytes: 2097152, shared_bytes: 209715200 },
    { id: 'fork-a1', parent_id: 'fork-a', phase: 'Running', private_dirty_bytes: 1048576, shared_bytes: 212860928 },
  ],
}
```

- [ ] **Step 5: Run it and confirm it passes**

Run: `pnpm -C web/app test src/data/forktree.test.tsx`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add web/app/src/api.ts web/app/src/data/forktree.ts web/app/src/data/forktree.test.tsx
git commit -s -m "feat(console): add fork-tree client types, hook, and dev fixture"
```

---

### Task 6: Fork-tree layout (pure function)

**Files:**
- Create: `web/app/src/views/forktree/layout.ts`
- Test: `web/app/src/views/forktree/layout.test.ts`

**Interfaces:**
- Consumes: `ForkNode` from `api.ts`.
- Produces:
  - `type PositionedNode = ForkNode & { x: number; y: number; depth: number }`.
  - `type Edge = { from: string; to: string }`.
  - `type Layout = { nodes: PositionedNode[]; edges: Edge[]; width: number; height: number }`.
  - `layoutForkTree(nodes: ForkNode[], opts?: { width?: number; levelHeight?: number }): Layout` - a deterministic top-down tiered layout (roots at depth 0; children spread horizontally under their parent). Pure and side-effect free so it is unit-tested without rendering.

- [ ] **Step 1: Write the failing test `web/app/src/views/forktree/layout.test.ts`**

```ts
import { describe, it, expect } from 'vitest'
import { layoutForkTree } from './layout'
import { FORK_TREE_FIXTURE } from '../../data/forktree'

describe('layoutForkTree', () => {
  it('places the root at depth 0 and children deeper', () => {
    const l = layoutForkTree(FORK_TREE_FIXTURE.nodes, { width: 600, levelHeight: 100 })
    const root = l.nodes.find((n) => n.id === 'root')!
    const forkA = l.nodes.find((n) => n.id === 'fork-a')!
    const forkA1 = l.nodes.find((n) => n.id === 'fork-a1')!
    expect(root.depth).toBe(0)
    expect(forkA.depth).toBe(1)
    expect(forkA1.depth).toBe(2)
    expect(forkA.y).toBeGreaterThan(root.y)
    expect(forkA1.y).toBeGreaterThan(forkA.y)
  })

  it('produces one edge per non-root node', () => {
    const l = layoutForkTree(FORK_TREE_FIXTURE.nodes)
    expect(l.edges).toHaveLength(4) // 5 nodes, 1 root
    expect(l.edges).toContainEqual({ from: 'root', to: 'fork-a' })
  })

  it('is deterministic (no Math.random)', () => {
    const a = layoutForkTree(FORK_TREE_FIXTURE.nodes)
    const b = layoutForkTree(FORK_TREE_FIXTURE.nodes)
    expect(a).toEqual(b)
  })
})
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `pnpm -C web/app test src/views/forktree/layout.test.ts`
Expected: FAIL with "Cannot find module './layout'".

- [ ] **Step 3: Implement `web/app/src/views/forktree/layout.ts`**

```ts
// Pure tiered layout for the fork tree: roots at depth 0, each child placed one
// level below its parent and spread horizontally across the available width by
// depth. Deterministic (no randomness, no clock) so it is fully unit-testable and
// the rendering stays a thin function of these positions.
import type { ForkNode } from '../../api'

export type PositionedNode = ForkNode & { x: number; y: number; depth: number }
export type Edge = { from: string; to: string }
export type Layout = { nodes: PositionedNode[]; edges: Edge[]; width: number; height: number }

export function layoutForkTree(
  nodes: ForkNode[],
  opts: { width?: number; levelHeight?: number } = {},
): Layout {
  const width = opts.width ?? 800
  const levelHeight = opts.levelHeight ?? 120

  const byId = new Map(nodes.map((n) => [n.id, n]))
  const depthOf = new Map<string, number>()
  const depth = (id: string): number => {
    if (depthOf.has(id)) return depthOf.get(id)!
    const n = byId.get(id)
    const d = !n || !n.parent_id || !byId.has(n.parent_id) ? 0 : depth(n.parent_id) + 1
    depthOf.set(id, d)
    return d
  }

  const tiers = new Map<number, ForkNode[]>()
  for (const n of nodes) {
    const d = depth(n.id)
    const t = tiers.get(d) ?? []
    t.push(n)
    tiers.set(d, t)
  }

  const positioned: PositionedNode[] = []
  for (const [d, tierNodes] of [...tiers.entries()].sort((a, b) => a[0] - b[0])) {
    const step = width / (tierNodes.length + 1)
    tierNodes.forEach((n, i) => {
      positioned.push({ ...n, depth: d, x: step * (i + 1), y: levelHeight * (d + 1) })
    })
  }

  const edges: Edge[] = nodes
    .filter((n) => n.parent_id && byId.has(n.parent_id))
    .map((n) => ({ from: n.parent_id, to: n.id }))

  const maxDepth = Math.max(0, ...positioned.map((n) => n.depth))
  return { nodes: positioned, edges, width, height: levelHeight * (maxDepth + 2) }
}
```

- [ ] **Step 4: Run it and confirm it passes**

Run: `pnpm -C web/app test src/views/forktree/layout.test.ts`
Expected: PASS, 3 tests.

- [ ] **Step 5: Commit**

```bash
git add web/app/src/views/forktree/layout.ts web/app/src/views/forktree/layout.test.ts
git commit -s -m "feat(console): add deterministic fork-tree layout function"
```

---

### Task 7: Fork-tree visualization + accessible table

**Files:**
- Create: `web/app/src/views/forktree/ForkTree.tsx`
- Create: `web/app/src/views/forktree/ForkTree.test.tsx`
- Create: `web/app/src/views/forktree/ForkTree.a11y.test.tsx`
- Modify: `web/packages/brand/src/base.css` (fork-tree node styles)

**Interfaces:**
- Consumes: `useForkTree` (Task 5), `layoutForkTree` (Task 6), `Skeleton`/`EmptyState` (B0), `useNavigate` from `@tanstack/react-router`, `fmtBytes` from `api.ts`.
- Produces: the `ForkTree` view component (default-named export `ForkTree`).

This task renders the layout as the brand `Division` motif: the parent snapshot as a cyan genome, forks as magenta membranes, each node sized/labelled by its private-dirty vs shared CoW set. The SVG is decorative; a parallel, screen-reader-accessible data table is the accessible source of truth (every node and its CoW stats), so the view is fully usable without sight. The interface-design skill drives the SVG craft during execution; the tests below pin behavior and accessibility.

- [ ] **Step 1: Write the failing behavior test `web/app/src/views/forktree/ForkTree.test.tsx`**

```tsx
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { renderAt } from '../../test/utils'
import type { Capabilities } from '../../api'

const caps: Capabilities = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) {
      return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/forktree')) {
      return Promise.resolve(new Response(JSON.stringify({ org_id: 'o1', nodes: [
        { id: 'root', parent_id: '', phase: 'Running', private_dirty_bytes: 0, shared_bytes: 209715200 },
        { id: 'fork-a', parent_id: 'root', phase: 'Running', private_dirty_bytes: 3145728, shared_bytes: 209715200 },
      ] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('ForkTree view', () => {
  it('renders every node in the accessible table', async () => {
    await renderAt('/forks', caps)
    await waitFor(() => expect(screen.getByRole('table', { name: /fork tree/i })).toBeInTheDocument())
    expect(screen.getByRole('row', { name: /root/i })).toBeInTheDocument()
    expect(screen.getByRole('row', { name: /fork-a/i })).toBeInTheDocument()
  })

  it('navigates when a node row is activated', async () => {
    const user = userEvent.setup()
    await renderAt('/forks', caps)
    await waitFor(() => expect(screen.getByRole('table', { name: /fork tree/i })).toBeInTheDocument())
    const link = screen.getByRole('link', { name: /fork-a/i })
    await user.click(link)
    // The link points at the sandbox; assert it carries the sandbox id in href.
    expect(link).toHaveAttribute('href', expect.stringContaining('fork-a'))
  })
})
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `pnpm -C web/app test src/views/forktree/ForkTree.test.tsx`
Expected: FAIL (no `/forks` route yet, and the component does not exist). You will add the route in Task 8; for this task, temporarily test the component directly if the route is not yet wired, then switch to the route once Task 8 lands. To keep this task self-contained, render the component under a minimal router in the test instead of `renderAt('/forks')`: wrap `<ForkTree/>` in the existing `renderWithQuery` helper plus a `RouterProvider` is unnecessary; instead assert the table and rows without navigation, and move the navigation assertion to Task 8. (Adjust the test to render `<ForkTree/>` directly via `renderWithQuery`.)

Note to implementer: prefer rendering `<ForkTree/>` directly through a query+router test harness in this task; if `useNavigate` requires a router context, use a tiny memory router wrapper. Keep the table + rows assertions here; the route-level navigation assertion belongs in Task 8.

- [ ] **Step 3: Implement `web/app/src/views/forktree/ForkTree.tsx`**

Render: a header; on load, `Skeleton`; on empty nodes, an `EmptyState` ("No forks yet. Fork a sandbox to see its copy-on-write tree."); otherwise an SVG of the laid-out tree (edges as lines, nodes as the `Division`-styled circles sized by `private_dirty_bytes`, the root in cyan, forks in magenta, with `<title>`/`aria-hidden` on the SVG) AND a visually secondary but screen-reader-complete `<table aria-label="Fork tree">` with one row per node: id (a `Link` to `/sandboxes/{id}`), parent, phase, private-dirty (`fmtBytes`), shared (`fmtBytes`). The table is the accessible source of truth; the SVG is `aria-hidden`. Use `var(--*)` tokens for all colors. Keep the component a thin function of `layoutForkTree(data.nodes)`.

(The implementer writes the full component with the interface-design skill; the tests pin the table, rows, links, and accessibility.)

- [ ] **Step 4: Run the behavior test and confirm it passes**

Run: `pnpm -C web/app test src/views/forktree/ForkTree.test.tsx`
Expected: PASS.

- [ ] **Step 5: Write the a11y test `web/app/src/views/forktree/ForkTree.a11y.test.tsx`**

```tsx
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import { axe } from 'vitest-axe'
import * as matchers from 'vitest-axe/matchers'
import { renderWithQuery } from '../../test/utils'
import { ForkTree } from './ForkTree'

expect.extend(matchers)

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockResolvedValue(
    new Response(JSON.stringify({ org_id: 'o1', nodes: [
      { id: 'root', parent_id: '', phase: 'Running', private_dirty_bytes: 0, shared_bytes: 209715200 },
      { id: 'fork-a', parent_id: 'root', phase: 'Running', private_dirty_bytes: 3145728, shared_bytes: 209715200 },
    ] }), { status: 200, headers: { 'content-type': 'application/json' } }),
  )
})

describe('ForkTree accessibility', () => {
  it('has no axe violations and exposes the data as a table', async () => {
    const { container } = renderWithQuery(<ForkTreeRouterHarness />)
    await waitFor(() => expect(screen.getByRole('table', { name: /fork tree/i })).toBeInTheDocument())
    expect(await axe(container)).toHaveNoViolations()
  })
})
```

(`ForkTreeRouterHarness` is a tiny wrapper the implementer adds in the test that provides a router context for the `Link`s; or the implementer uses the existing router test helper. Keep it minimal.)

- [ ] **Step 6: Run the a11y test; fix any violations**

Run: `pnpm -C web/app test src/views/forktree/ForkTree.a11y.test.tsx`
Expected: PASS. Fix any reported violation in the component (table headers, link names, SVG `aria-hidden`); do not suppress rules.

- [ ] **Step 7: Add fork-tree node styles to `web/packages/brand/src/base.css`**

Append token-driven styles for `.fork-node-root` (cyan), `.fork-node-fork` (magenta), `.fork-edge` (hairline stroke), and a `@media (max-width: 768px)` rule so the SVG scales/scrolls and the table remains the primary representation on small screens. No raw hex.

- [ ] **Step 8: Commit**

```bash
git add web/app/src/views/forktree/ForkTree.tsx web/app/src/views/forktree/ForkTree.test.tsx web/app/src/views/forktree/ForkTree.a11y.test.tsx web/packages/brand/src/base.css
git commit -s -m "feat(console): add the live fork-tree visualization with an accessible table"
```

---

### Task 8: Route the fork-tree hero view + final verification

**Files:**
- Modify: `web/app/src/nav/routes.tsx` (add the `/forks` route)
- Modify: `web/app/src/views/forktree/ForkTree.test.tsx` (add the route-level navigation assertion deferred from Task 7)

**Interfaces:**
- Consumes: `ForkTree` view (Task 7), the routes config (B0).
- Produces: a `/forks` route in group `Run` labelled "Fork tree".

- [ ] **Step 1: Write the failing route test (extend `ForkTree.test.tsx` with a route-level case)**

```tsx
describe('ForkTree route', () => {
  it('mounts at /forks and a node links to its sandbox', async () => {
    const user = userEvent.setup()
    await renderAt('/forks', caps) // caps + fetch mock from the top of this file
    await waitFor(() => expect(screen.getByRole('table', { name: /fork tree/i })).toBeInTheDocument())
    const link = screen.getByRole('link', { name: /fork-a/i })
    expect(link).toHaveAttribute('href', expect.stringContaining('fork-a'))
  })
})
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `pnpm -C web/app test src/views/forktree/ForkTree.test.tsx`
Expected: FAIL (no `/forks` route mounts the view yet, so the table is not found at that path).

- [ ] **Step 3: Add the route in `web/app/src/nav/routes.tsx`**

Import the view and add a route to the `ROUTES` array in the `Run` group, after `/sandboxes`:

```tsx
import { ForkTree } from '../views/forktree/ForkTree'
```

```tsx
  { path: '/forks', label: 'Fork tree', group: 'Run', element: () => <ForkTree /> },
```

- [ ] **Step 4: Run the route test and confirm it passes**

Run: `pnpm -C web/app test src/views/forktree/ForkTree.test.tsx`
Expected: PASS.

- [ ] **Step 5: Full verification**

Run: `pnpm -C web/app test` (exit 0, all unit + a11y tests pass)
Run: `pnpm -C web/app typecheck` (clean)
Run: `pnpm -C web/app build` (succeeds, dist emitted)
Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/saas/console/` (Go BFF tests pass)
Run: `golangci-lint run --timeout=5m ./internal/saas/console/... && GOOS=linux golangci-lint run --timeout=5m ./internal/saas/console/...` (clean both ways)
Run: `grep -rnP '\xe2\x80\x94|\xe2\x80\x93' web/app/src/views/forktree web/app/src/views/Instruments.tsx web/app/src/ui/StatTile.tsx internal/saas/console/forktree.go` (empty)

- [ ] **Step 6: Commit**

```bash
git add web/app/src/nav/routes.tsx web/app/src/views/forktree/ForkTree.test.tsx
git commit -s -m "feat(console): route the fork-tree hero view at /forks"
```

---

## Self-Review

**Spec coverage (sections 4.4, 9 of the spec):**
- Instruments cockpit reading the org's measured #211/#33 telemetry: Tasks 1, 2, 3. Covered.
- "Reproduce this" affordance on every headline metric pointing at the in-repo bench: Task 2 (`StatTile.reproduce`), Task 3 (wired on each tile). Covered.
- Honest empty state ("only measured signal emits light"): Task 3. Covered.
- Live fork tree as the `Division` motif, annotated by private-dirty vs shared CoW pages: Tasks 4 (data: `PrivateDirtyBytes`/`SharedBytes`), 6 (layout), 7 (visualization). Covered.
- The integrity rule (no fabricated numbers; values only from the live system): enforced by the Global Constraints and by reading `/console/instruments` and `/console/forktree`; no metric literal in UI source. Covered.
- Org-scoped isolation for the new endpoint: Task 4 (`TestForkTreeIsOrgScoped`, cross-org node never leaks). Covered.
- Responsive + accessibility (spec 4.6): cockpit grid reflows (Task 3 CSS), fork tree has a screen-reader table and an axe test (Task 7), node styles reflow on mobile (Task 7 CSS). Covered.

**Deferred (later plans, not here):** the real controller-backed `ForkTreeSource` that walks husk-pod CoW lineage and #33 metering (a Go follow-up, like `SandboxControl`'s real implementation; B1 ships the seam + fake + endpoint, so the UI is real and tested now); the per-sandbox fork-tree detail tab and the Sandboxes detail view (B2); trend sparklines on `StatTile` (only added when a time-series source exists, per YAGNI).

**Placeholder scan:** no "TBD"/"TODO"/"implement later"; the one prose-only implementation step (Task 7, Step 3, the SVG component) is a deliberate craft step with its behavior and accessibility pinned by Steps 1, 5 and the interface-design skill, not a placeholder; every other code step shows complete code.

**Type consistency:** `Instruments` from `api.ts` is used unchanged in Tasks 1, 3; `ForkNode`/`ForkTree` defined in Task 5 (client) mirror the Go `ForkNode`/`ForkTree` JSON tags from Task 4 (`private_dirty_bytes`, `shared_bytes`, `parent_id`); `layoutForkTree` (Task 6) consumes `ForkNode` and is consumed by Task 7; `useForkTree`/`FORK_TREE_FIXTURE` (Task 5) are consumed by Tasks 6 (fixture in tests) and 7. The Go `ForkTreeSource`/`Deps.ForkTree`/`handleForkTree` names are consistent across Task 4. No drift.
