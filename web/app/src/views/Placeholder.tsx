// A friendly stand-in for a page that is still being built. It says what the
// page will do, that it is not available yet, and points at one concrete way
// to get the same job done today. It never names internal endpoints or
// roadmap phases: a teaching state, not a dead end.
import type { ReactNode } from 'react'
import { PageHeader } from '../ui/PageHeader'

export function Placeholder({
  title,
  description,
  today,
}: {
  title: string
  /** What this page will let the user do, in plain language. */
  description: string
  /** One concrete way to do the same thing right now. */
  today?: ReactNode
}) {
  return (
    <section>
      <PageHeader title={title} lede={description} />
      <p className="t-dim">This page is not available yet.</p>
      {today && <p className="t-dim">{today}</p>}
    </section>
  )
}
