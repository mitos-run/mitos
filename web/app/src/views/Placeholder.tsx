// Honest placeholder for a route whose rich view ships in a later phase. It
// names the org-scoped BFF endpoint that already backs it, so the shell never
// lies about what is wired.
export function Placeholder({ title, endpoint, phase }: { title: string; endpoint: string; phase: string }) {
  return (
    <section>
      <h2>{title}</h2>
      <p className="t-dim">
        Reads <code>{endpoint}</code>. The rich view ships in {phase}. It reads the org-scoped BFF endpoint named above.
      </p>
    </section>
  )
}
