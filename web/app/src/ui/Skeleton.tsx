// Skeleton placeholder: shown while a query is loading so navigation reveals
// structure instantly instead of a spinner. Pure presentational.
export function Skeleton({ rows = 3 }: { rows?: number }) {
  return (
    <div aria-busy="true" aria-label="loading">
      {Array.from({ length: rows }).map((_, i) => (
        <div key={i} className="skeleton-row" style={{ height: 'var(--space-5)', marginBottom: 'var(--space-2)', borderRadius: 'var(--r-sm)', background: 'var(--field-1)' }} />
      ))}
    </div>
  )
}
