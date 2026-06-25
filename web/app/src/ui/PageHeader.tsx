// The uniform page header: every view opens with the same zone so the app reads
// as one product. An optional eyebrow (a short kicker or group name), the page
// title as the single h1, an optional one-line lede, and an optional right
// aligned actions slot for the page's primary action. Mirrors the marketing
// site's .page-hero rhythm so the website to app handoff feels continuous.
import type { ReactNode } from 'react'

export function PageHeader({ eyebrow, title, lede, actions }: {
  eyebrow?: string
  title: string
  lede?: string
  actions?: ReactNode
}) {
  return (
    <header className="page-header">
      <div className="page-header-text">
        {eyebrow && <div className="page-header-eyebrow">{eyebrow}</div>}
        <h1 className="page-header-title">{title}</h1>
        {lede && <p className="page-header-lede t-dim">{lede}</p>}
      </div>
      {actions && <div className="page-header-actions">{actions}</div>}
    </header>
  )
}
