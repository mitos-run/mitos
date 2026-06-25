// A button styled as a search input. Clicking it (or Cmd-K, handled by AppShell)
// opens the command palette. Making search visible is what makes the palette
// discoverable to new users.
export function SearchTrigger({ onClick }: { onClick: () => void }) {
  return (
    <button type="button" className="search-trigger" aria-label="Search (Cmd K)" onClick={onClick}>
      <svg width="15" height="15" viewBox="0 0 16 16" fill="none" aria-hidden="true" focusable="false">
        <circle cx="7" cy="7" r="5" stroke="currentColor" strokeWidth="1.5" />
        <path d="M11 11l3.5 3.5" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
      </svg>
      <span className="search-trigger-label">Search</span>
      <kbd className="search-trigger-kbd">Cmd K</kbd>
    </button>
  )
}
