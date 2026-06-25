// Teaching empty state: never a blank panel. A title, a one-line explanation,
// and an optional primary action that starts the thing the view is for.
import { Button } from '@mitos/brand'

export function EmptyState({
  title,
  body,
  action,
}: {
  title: string
  body: string
  action?: { label: string; onClick: () => void }
}) {
  return (
    <div className="card" style={{ textAlign: 'center', padding: 'var(--space-8)' }}>
      <h2 style={{ marginBottom: 'var(--space-2)' }}>{title}</h2>
      <p className="t-dim" style={{ marginBottom: 'var(--space-5)' }}>{body}</p>
      {action && <Button onClick={action.onClick}>{action.label}</Button>}
    </div>
  )
}
