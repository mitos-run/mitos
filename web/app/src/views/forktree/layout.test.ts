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
