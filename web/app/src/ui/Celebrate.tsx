// Brand-aligned first-run celebration: a burst of brand-colored pieces on
// mount, reduced to a calm accessible status announcement under
// prefers-reduced-motion. No third-party confetti library.
// Fluorescence tokens only; no hardcoded hex. No em/en dashes.

// ---- Page-specific styles ---------------------------------------------------

const styles = `
@keyframes celebrate-fly {
  from {
    transform: translateY(0) rotate(0deg);
    opacity: 1;
  }
  to {
    transform: translateY(-140px) rotate(720deg);
    opacity: 0;
  }
}
.celebrate-burst {
  position: fixed;
  inset: 0;
  pointer-events: none;
  z-index: 9999;
  overflow: hidden;
}
.celebrate-piece {
  position: absolute;
  width: 8px;
  height: 8px;
  border-radius: var(--r-sm, 2px);
  animation: celebrate-fly 1.4s var(--ease, ease-out) forwards;
}
`

// Pieces alternate between the two brand accent colors.
const COLORS = ['var(--cyan)', 'var(--magenta)']
const PIECE_COUNT = 18

// Deterministic-looking spread without seeded random: distribute pieces across
// the viewport with enough variance to look natural.
function buildPieces() {
  const pieces: Array<{ left: number; top: number; delay: number; color: string }> = []
  for (let i = 0; i < PIECE_COUNT; i++) {
    pieces.push({
      left: 5 + (i * 94) / (PIECE_COUNT - 1),   // spread 5..99 across pieces
      top: 20 + (((i * 37) % 60)),               // vary top 20..80
      delay: (i * 0.4) / PIECE_COUNT,            // stagger 0..0.4s
      color: COLORS[i % COLORS.length],
    })
  }
  return pieces
}

const PIECES = buildPieces()

// ---- Component --------------------------------------------------------------

export function Celebrate({ active }: { active: boolean }): JSX.Element | null {
  if (!active) return null

  const reducedMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches

  return (
    <>
      <style>{styles}</style>
      {/* Visible status announcement; read immediately by screen readers. */}
      <div
        role="status"
        aria-live="polite"
        style={{ textAlign: 'center', padding: 'var(--space-3) 0', color: 'var(--cyan)' }}
      >
        You are live
      </div>
      {!reducedMotion && (
        <div aria-hidden="true" data-testid="confetti-burst" className="celebrate-burst">
          {PIECES.map((p, i) => (
            <span
              key={i}
              className="celebrate-piece"
              style={{
                left: `${p.left}%`,
                top: `${p.top}%`,
                background: p.color,
                animationDelay: `${p.delay}s`,
              }}
            />
          ))}
        </div>
      )}
    </>
  )
}
