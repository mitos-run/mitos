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
