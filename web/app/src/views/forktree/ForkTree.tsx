// ForkTree: the brand Division motif rendered as a live SVG copy-on-write
// tree. The SVG is decorative (aria-hidden); a parallel accessible table is
// the screen-reader source of truth. Layout is a pure function of
// layoutForkTree(nodes) so this component is a thin renderer over positions.
import { Link } from '@tanstack/react-router'
import { useForkTree } from '../../data/forktree'
import { fmtBytes } from '../../api'
import { Skeleton } from '../../ui/Skeleton'
import { EmptyState } from '../../ui/EmptyState'
import { layoutForkTree, type PositionedNode } from './layout'

// Minimum and maximum visual radius for a node circle. Size encodes
// private-dirty bytes: root (0 bytes) gets MIN_R, heavily written forks get
// up to MAX_R. A modest logarithmic scale avoids huge nodes dominating.
const MIN_R = 10
const MAX_R = 28

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

  if (isError) {
    return (
      <section aria-label="Fork tree" style={{ padding: 'var(--space-5)' }}>
        <h2 style={{ marginBottom: 'var(--space-4)' }}>Fork tree</h2>
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
        <h2 style={{ marginBottom: 'var(--space-4)' }}>Fork tree</h2>
        <Skeleton rows={4} />
      </section>
    )
  }

  if (!data || data.nodes.length === 0) {
    return (
      <section aria-label="Fork tree" style={{ padding: 'var(--space-5)' }}>
        <h2 style={{ marginBottom: 'var(--space-4)' }}>Fork tree</h2>
        <EmptyState
          title="No forks yet"
          body="Fork a sandbox to see its copy-on-write tree."
        />
      </section>
    )
  }

  const layout = layoutForkTree(data.nodes)
  const maxDirty = Math.max(0, ...data.nodes.map((n) => n.private_dirty_bytes))

  return (
    <section aria-label="Fork tree" style={{ padding: 'var(--space-5)' }}>
      <h2 style={{ marginBottom: 'var(--space-4)' }}>Fork tree</h2>

      {/* SVG visualization: decorative, hidden from screen readers. The table
          below is the accessible source of truth for assistive technology. */}
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

          {/* Nodes: circles sized by private_dirty_bytes */}
          {layout.nodes.map((node) => {
            const r = nodeRadius(node, maxDirty)
            const cls = nodeClass(node)
            return (
              <g key={node.id}>
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
          of truth; the SVG above is aria-hidden and adds no information. */}
      <div style={{ overflowX: 'auto' }}>
        <table className="tbl" aria-label="Fork tree nodes">
          <thead>
            <tr>
              <th scope="col">ID</th>
              <th scope="col">Parent</th>
              <th scope="col">Phase</th>
              <th scope="col">Private dirty</th>
              <th scope="col">Shared</th>
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
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  )
}

export default ForkTree
