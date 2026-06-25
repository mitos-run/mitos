// Honest placeholder for a route whose rich view ships in a later phase. It
// names the org-scoped BFF endpoint that already backs it, so the shell never
// lies about what is wired.
import { PageHeader } from '../ui/PageHeader'

export function Placeholder({ title, endpoint, phase }: { title: string; endpoint: string; phase: string }) {
  return (
    <section>
      <PageHeader title={title} />
      <p className="t-dim">
        Reads <code>{endpoint}</code>. The rich view ships in {phase}. It reads the org-scoped BFF endpoint named above.
      </p>
    </section>
  )
}
