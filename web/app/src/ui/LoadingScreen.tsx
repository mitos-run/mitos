// Full-page loading state shown before the shell (or its chrome) can render:
// the same Skeleton rows every view uses, in a page-width column, so boot
// looks like the product settling in rather than bare placeholder text.
import { Skeleton } from './Skeleton'

export function LoadingScreen() {
  return (
    <main style={{ maxWidth: 'var(--maxw)', margin: '0 auto', padding: 'var(--space-6)' }}>
      <Skeleton rows={4} />
    </main>
  )
}
