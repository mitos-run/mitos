// Templates view: table listing org templates with columns Name, Image,
// Description, Updated. Empty state when none exist. Consumes the live BFF
// via useTemplates().
import { useTemplates } from '../data/account'
import { Skeleton } from '../ui/Skeleton'
import { EmptyState } from '../ui/EmptyState'
import { PageHeader } from '../ui/PageHeader'
import { TableToolbar, useTableFilter } from '../ui/TableToolbar'

export function Templates() {
  const { data: templates = [], isLoading } = useTemplates()
  const { query, setQuery, filtered } = useTableFilter(templates, (t) => `${t.name} ${t.description}`)

  return (
    <section>
      <PageHeader title="Templates" lede="Preconfigured sandbox images available to this org." />

      {isLoading ? (
        <Skeleton rows={3} />
      ) : templates.length === 0 ? (
        <EmptyState
          title="No templates yet"
          body="Templates registered in this org will appear here."
        />
      ) : (
        <div style={{ overflowX: 'auto' }}>
          <TableToolbar query={query} onQueryChange={setQuery} count={filtered.length} noun="templates" />
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
              {filtered.map((t) => (
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
