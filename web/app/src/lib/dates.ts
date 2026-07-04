// Shared date formatting so every view renders timestamps the same way,
// instead of the scattered toLocale*() calls that used to be sprinkled across
// Settings, Members, Keys, Templates, Projects, and Billing (each rendering a
// timestamp slightly differently: some locale-aware, some not, some date-only,
// some date+time). fmtRelative gives a short, glanceable form for recent
// events; fmtAbsolute is the precise, locale/timezone-aware form everything
// falls back to.

const MINUTE_MS = 60 * 1000
const HOUR_MS = 60 * MINUTE_MS
const DAY_MS = 24 * HOUR_MS
const RELATIVE_CUTOFF_MS = 7 * DAY_MS

/** fmtRelative renders iso as a short relative string ("15m ago", "3h ago",
 * "2d ago") for anything less than 7 days old (and in the past); anything
 * older, in the future, or unparseable falls back to fmtAbsolute(iso) using
 * browser defaults (this function takes no locale/timezone, matching the
 * console's audit table and recent-activity panel, where compactness matters
 * more than a caller-specific format for old events). */
export function fmtRelative(iso: string): string {
  const d = new Date(iso)
  if (isNaN(d.getTime())) return iso
  const diffMs = Date.now() - d.getTime()
  if (diffMs < 0 || diffMs >= RELATIVE_CUTOFF_MS) {
    return fmtAbsolute(iso)
  }
  const minutes = Math.floor(diffMs / MINUTE_MS)
  if (minutes < 1) return 'just now'
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.floor(diffMs / HOUR_MS)
  if (hours < 24) return `${hours}h ago`
  const days = Math.floor(diffMs / DAY_MS)
  return `${days}d ago`
}

/** fmtAbsolute renders iso as a locale/timezone-aware date+time string.
 * locale and tz default to the browser's own when omitted (the caller has no
 * saved preference yet, or is rendering before the account has loaded). An
 * unparseable iso is returned unchanged rather than rendering "Invalid Date". */
export function fmtAbsolute(iso: string, locale?: string, tz?: string): string {
  const d = new Date(iso)
  if (isNaN(d.getTime())) return iso
  try {
    return d.toLocaleString(locale || undefined, {
      timeZone: tz || undefined,
      year: 'numeric',
      month: 'short',
      day: 'numeric',
      hour: '2-digit',
      minute: '2-digit',
    })
  } catch {
    // An unknown/invalid locale or IANA timezone string (e.g. a malformed
    // saved preference) falls back to the browser's own default formatting
    // rather than throwing and blanking the cell.
    return d.toLocaleString()
  }
}
