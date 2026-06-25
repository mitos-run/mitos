// Templates view: table listing org templates with columns Name, Image,
// Description, Updated. Empty state when none exist. Consumes the live BFF
// via useTemplates().
import { useTemplates } from '../data/account'
import { Skeleton } from '../ui/Skeleton'
import { EmptyState } from '../ui/EmptyState'

export function Templates() {
  const { data: templates = [], isLoading } = useTemplates()

  return (
    <section>
      <h2>Templates</h2>
      <p className="t-dim" style={{ fontSize: 'var(--step--1)', marginBottom: 'var(--space-5)' }}>
        Preconfigured sandbox images available to this org.
      </p>

      {isLoading ? (
        <Skeleton rows={3} />
      ) : templates.length === 0 ? (
        <EmptyState
          title="No templates yet"
          body="Templates registered in this org will appear here."
        />
      ) : (
        <div style={{ overflowX: 'auto' }}>
          <table className="tbl" aria-label="Templates">
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Image</th>
                <th scope="col">Description</th>
                <th scope="col">Updated</th>
              </tr>
            </thead>
            <tbody>
              {templates.map((t) => (
                <tr key={t.name}>
                  <td className="mono">{t.name}</td>
                  <td className="mono t-dim">{t.image}</td>
                  <td>{t.description}</td>
                  <td className="t-dim">{new Date(t.updated_at).toLocaleDateString()}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}
