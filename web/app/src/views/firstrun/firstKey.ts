// firstKey.ts: one-time first-key sessionStorage helpers for the console first-run.
//
// The raw API key is a secret. It is held here only in sessionStorage for the
// duration of the single browser session started by email verification. Once
// the first-run reads it via takeFirstKey(), it is gone. It is NEVER logged,
// NEVER rendered raw in the first-run UI (only the masked prefix is shown),
// and NEVER stored anywhere more durable than sessionStorage.

const STORAGE_KEY = 'mitos.firstKey'

/**
 * peekFirstKey returns the raw first API key from sessionStorage without
 * removing it, or null if none is stored. Use this when you need to check
 * presence without consuming the value.
 */
export function peekFirstKey(): string | null {
  return sessionStorage.getItem(STORAGE_KEY)
}

/**
 * takeFirstKey returns the raw first API key from sessionStorage AND removes
 * it, so a subsequent peek returns null. This is the one-time-read guarantee:
 * after the first-run takes the key it is gone from the browser store.
 */
export function takeFirstKey(): string | null {
  const value = sessionStorage.getItem(STORAGE_KEY)
  if (value !== null) {
    sessionStorage.removeItem(STORAGE_KEY)
  }
  return value
}

/**
 * maskKey returns a display-safe masked form of the raw key: the first 12
 * characters of the key followed by exactly 8 bullet characters (U+2022).
 * The raw tail after the prefix never appears in the output.
 */
export function maskKey(key: string): string {
  const prefix = key.slice(0, 12)
  return prefix + '•'.repeat(8)
}
