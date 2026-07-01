// Polls the console first-activity endpoint until the new user's first exec
// lands (active: true), then stops polling. A later task renders a waiting
// state while active is false and transitions once it becomes true.
import { useQuery } from '@tanstack/react-query'
import { firstActivity } from '../api'

export function useFirstActivity(enabled: boolean) {
  return useQuery({
    queryKey: ['first-activity'],
    queryFn: firstActivity,
    enabled,
    refetchInterval: (q) => (q.state.data?.active ? false : 3000),
  })
}
