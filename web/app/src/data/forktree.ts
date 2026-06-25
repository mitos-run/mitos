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
