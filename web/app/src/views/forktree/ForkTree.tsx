// ForkTree: the brand Division motif rendered as a live SVG copy-on-write
// tree. The SVG is decorative (aria-hidden) and only mouse-clickable; a
// parallel accessible table is the screen-reader source of truth AND the
// keyboard entry point (a "Details" button per row, a real focusable button
// so Enter/Space just works) into the node detail side panel. Layout is a
// pure function of layoutForkTree(nodes) so this component is a thin renderer
// over positions.
import { useRef, useState, type RefObject } from 'react'
import { Link } from '@tanstack/react-router'
import { Button } from '@mitos/brand'
import { useForkTree } from '../../data/forktree'
import { useForkSandbox, useTerminateSandbox } from '../../data/sandboxes'
import { fmtBytes, type ForkNode } from '../../api'
import { Skeleton } from '../../ui/Skeleton'
import { EmptyState } from '../../ui/EmptyState'
import { useToast } from '../../ui/Toast'
import { PageHeader } from '../../ui/PageHeader'
import { useModalFocus } from '../../ui/useModalFocus'
import { NewSandboxModal } from '../sandboxes/NewSandboxModal'
import { layoutForkTree, type PositionedNode } from './layout'

const MAX_FORK_COUNT = 16

// NodeDetailPanel: the side panel a selected node opens. Shows id/phase/
// private+shared bytes and the Fork n / Open / Terminate actions the brief
// calls for. Rendered as a labelled region so it is announced when it
// appears; focus moves to the heading on mount (mirrors FeedbackButton's
// textarea-focus-on-open pattern) so a keyboard/screen-reader user gets a
// clear signal the panel opened. This is a panel, not a modal (no Tab trap:
// the rest of the page stays reachable), but it shares the same
// open/close focus plumbing as every dialog via useModalFocus: on unmount,
// focus returns to the "Details" button (returnFocusRef, set by the caller)
// that opened it.
function NodeDetailPanel({
  node,
  returnFocusRef,
  onClose,
}: {
  node: ForkNode
  returnFocusRef: RefObject<HTMLButtonElement | null>
  onClose: () => void
}) {
  const [count, setCount] = useState(2)
  const fork = useForkSandbox()
  const terminate = useTerminateSandbox()
  const { notify } = useToast()
  const headingRef = useRef<HTMLHeadingElement>(null)
  const panelRef = useRef<HTMLElement>(null)

  useModalFocus(panelRef, { active: true, initialFocusRef: headingRef, returnFocusRef, trap: false })

  function onFork() {
    fork.mutate(
      { id: node.id, count },
      {
        onSuccess: (res) => notify(`Forked ${node.id} into ${res.ids.length}`, 'ok'),
        onError: (err) => notify(err instanceof Error ? err.message : `Failed to fork ${node.id}`, 'error'),
      },
    )
  }

  function onTerminate() {
    terminate.mutate(node.id, {
      onSuccess: () => {
        notify(`Terminated ${node.id}`, 'ok')
        onClose()
      },
      onError: () => notify(`Failed to terminate ${node.id}`, 'error'),
    })
  }

  return (
    <aside
      ref={panelRef}
      role="region"
      aria-label={`Details for sandbox ${node.id}`}
      className="card fork-node-panel"
      style={{ padding: 'var(--space-5)', marginBottom: 'var(--space-5)' }}
    >
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: 'var(--space-4)' }}>
        <h3 ref={headingRef} tabIndex={-1} className="mono" style={{ margin: 0 }}>{node.id}</h3>
        <button type="button" className="btn btn-ghost" onClick={onClose} aria-label="Close details">
          Close
        </button>
      </div>
      <dl className="kv" style={{ marginBottom: 'var(--space-5)' }}>
        <div className="kv-row"><dt className="t-dim">Phase</dt><dd>{node.phase}</dd></div>
        <div className="kv-row"><dt className="t-dim">Private dirty</dt><dd className="mono">{fmtBytes(node.private_dirty_bytes)}</dd></div>
        <div className="kv-row"><dt className="t-dim">Shared</dt><dd className="mono">{fmtBytes(node.shared_bytes)}</dd></div>
      </dl>
      <div style={{ display: 'flex', gap: 'var(--space-3)', alignItems: 'center', flexWrap: 'wrap' }}>
        <label htmlFor="fork-tree-fork-count" className="t-dim">Fork</label>
        <input
          id="fork-tree-fork-count"
          type="number"
          min={1}
          max={MAX_FORK_COUNT}
          value={count}
          onChange={(e) => setCount(Math.max(1, Math.min(MAX_FORK_COUNT, Number(e.target.value) || 1)))}
          style={{ width: '4rem' }}
        />
        <Button onClick={onFork} disabled={fork.isPending}>
          {fork.isPending ? 'Forking...' : `Fork ${count}`}
        </Button>
        <Link to="/sandboxes/$id" params={{ id: node.id }} className="btn btn-ghost">
          Open
        </Link>
        <button type="button" className="btn btn-ghost" onClick={onTerminate} disabled={terminate.isPending}>
          {terminate.isPending ? 'Terminating...' : 'Terminate'}
        </button>
      </div>
    </aside>
  )
}

// Minimum and maximum visual radius for a node circle. Size encodes
// private-dirty bytes: root (0 bytes) gets MIN_R, heavily written forks get
// up to MAX_R. A modest logarithmic scale avoids huge nodes dominating.
const MIN_R = 10
const MAX_R = 28

// Minimum invisible hit-target radius: half of the 44px touch-target
// recommendation, so a small (MIN_R = 10) node is still comfortably tappable
// on a phone/tablet even though its visible circle is much smaller.
const MIN_HIT_R = 22

function nodeRadius(node: PositionedNode, maxDirty: number): number {
  if (maxDirty <= 0 || node.private_dirty_bytes <= 0) return MIN_R
  const t = Math.log1p(node.private_dirty_bytes) / Math.log1p(maxDirty)
  return MIN_R + t * (MAX_R - MIN_R)
}

function nodeClass(node: PositionedNode): string {
  return node.parent_id === '' ? 'fork-node-root' : 'fork-node-fork'
}

export function ForkTree() {
  const { data, isLoading, isError } = useForkTree()
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [showNew, setShowNew] = useState(false)
  // Tracks the "Details" button that opened the panel; passed to
  // NodeDetailPanel as returnFocusRef so the shared useModalFocus hook
  // returns focus to the same trigger on close (keyboard/screen-reader
  // users get no signal otherwise that focus silently landed back on
  // <body>).
  const lastDetailsTriggerRef = useRef<HTMLButtonElement | null>(null)

  if (isError) {
    return (
      <section aria-label="Fork tree" style={{ padding: 'var(--space-5)' }}>
        <PageHeader title="Fork tree" />
        <EmptyState
          title="Fork tree unavailable"
          body="The fork tree could not be read for this organization."
        />
      </section>
    )
  }

  if (isLoading) {
    return (
      <section aria-label="Fork tree" style={{ padding: 'var(--space-5)' }}>
        <PageHeader title="Fork tree" />
        <Skeleton rows={4} />
      </section>
    )
  }

  if (!data || data.nodes.length === 0) {
    return (
      <section aria-label="Fork tree" style={{ padding: 'var(--space-5)' }}>
        <PageHeader title="Fork tree" />
        <EmptyState
          title="No forks yet"
          body="Start a sandbox, then fork it to see its copy-on-write tree here."
          action={{ label: 'New sandbox', onClick: () => setShowNew(true) }}
        />
        {showNew && <NewSandboxModal onClose={() => setShowNew(false)} />}
      </section>
    )
  }

  const layout = layoutForkTree(data.nodes)
  const maxDirty = Math.max(0, ...data.nodes.map((n) => n.private_dirty_bytes))
  const selectedNode = selectedId ? data.nodes.find((n) => n.id === selectedId) ?? null : null

  return (
    <section aria-label="Fork tree" style={{ padding: 'var(--space-5)' }}>
      <PageHeader title="Fork tree" />

      {selectedNode && (
        <NodeDetailPanel
          // Keyed by node.id: switching from one selected node straight to
          // another (without the panel closing in between) must re-run the
          // heading focus effect so focus/announcement moves again. Without
          // the key, React reuses the same component instance across the
          // node change, and the mount-only focus effect never re-fires.
          key={selectedNode.id}
          node={selectedNode}
          returnFocusRef={lastDetailsTriggerRef}
          onClose={() => setSelectedId(null)}
        />
      )}

      {/* SVG visualization: decorative and mouse-clickable only. It stays
          aria-hidden (a focusable-but-hidden element is a trap for keyboard/
          screen-reader users), so it never receives tabIndex; the table below
          is both the accessible source of truth AND the keyboard entry point
          into the same node detail panel via its "Details" button per row. */}
      <div
        className="fork-tree-svg-wrapper"
        style={{ overflowX: 'auto', marginBottom: 'var(--space-5)' }}
      >
        <svg
          aria-hidden="true"
          focusable="false"
          width={layout.width}
          height={layout.height}
          viewBox={`0 0 ${layout.width} ${layout.height}`}
          style={{ display: 'block', minWidth: '320px' }}
        >
          {/* Edges: hairline strokes from parent to child */}
          {layout.edges.map((edge) => {
            const fromNode = layout.nodes.find((n) => n.id === edge.from)
            const toNode = layout.nodes.find((n) => n.id === edge.to)
            if (!fromNode || !toNode) return null
            return (
              <line
                key={`${edge.from}-${edge.to}`}
                className="fork-edge"
                x1={fromNode.x}
                y1={fromNode.y}
                x2={toNode.x}
                y2={toNode.y}
              />
            )
          })}

          {/* Nodes: circles sized by private_dirty_bytes; a click selects the
              node for the detail panel (mouse-only affordance, decorative). */}
          {layout.nodes.map((node) => {
            const r = nodeRadius(node, maxDirty)
            const cls = nodeClass(node)
            return (
              <g key={node.id} onClick={() => setSelectedId(node.id)} style={{ cursor: 'pointer' }}>
                {/* Invisible hit-target circle: enlarges the tappable area to
                    at least a 44px diameter without changing the visible node
                    size, so small nodes stay comfortably tappable on touch. */}
                <circle
                  aria-hidden="true"
                  cx={node.x}
                  cy={node.y}
                  r={Math.max(r, MIN_HIT_R)}
                  fill="transparent"
                />
                <circle
                  className={cls}
                  cx={node.x}
                  cy={node.y}
                  r={r}
                />
                <text
                  x={node.x}
                  y={node.y + r + 14}
                  textAnchor="middle"
                  style={{
                    fontSize: 'var(--step--1)',
                    fill: 'var(--ink-2)',
                    fontFamily: 'var(--mono)',
                  }}
                >
                  {node.id}
                </text>
              </g>
            )
          })}
        </svg>
      </div>

      {/* Accessible table: one row per node. This is the screen-reader source
          of truth; the SVG above is aria-hidden and adds no information. The
          "Details" button is a real focusable button (Enter/Space just works)
          that opens the SAME side panel a mouse click on the SVG opens. */}
      <div style={{ overflowX: 'auto' }}>
        <table className="tbl" aria-label="Fork tree nodes">
          <thead>
            <tr>
              <th scope="col">ID</th>
              <th scope="col">Parent</th>
              <th scope="col">Phase</th>
              <th scope="col">Private dirty</th>
              <th scope="col">Shared</th>
              <th scope="col"><span className="sr-only">Details</span></th>
            </tr>
          </thead>
          <tbody>
            {data.nodes.map((node) => (
              <tr key={node.id}>
                <td>
                  {/* Deep-link to the sandbox detail view for this node. */}
                  <Link to="/sandboxes/$id" params={{ id: node.id }}>{node.id}</Link>
                </td>
                <td>{node.parent_id || '-'}</td>
                <td>{node.phase}</td>
                <td className="mono">{fmtBytes(node.private_dirty_bytes)}</td>
                <td className="mono">{fmtBytes(node.shared_bytes)}</td>
                <td>
                  <button
                    type="button"
                    className="btn btn-ghost"
                    onClick={(e) => {
                      lastDetailsTriggerRef.current = e.currentTarget
                      setSelectedId(node.id)
                    }}
                    aria-label={`View details for ${node.id}`}
                  >
                    Details
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  )
}

export default ForkTree
