// The global top bar: the single brand home, the mobile drawer toggle, a
// discoverable search trigger, a help link, a theme toggle, and the account
// menu. Styled to continue the marketing site nav (translucent 64px bar) so
// the website to app handoff is seamless.
import type { RefObject } from 'react'
import { Link } from '@tanstack/react-router'
import { Mark } from '@mitos/brand'
import type { Capabilities } from '../api'
import { SearchTrigger } from './SearchTrigger'
import { ThemeToggle } from './ThemeToggle'
import { FeedbackButton } from './FeedbackButton'
import { AccountMenu } from './AccountMenu'

export function TopBar({ caps, route, onSearch, onToggleDrawer, drawerOpen, menuButtonRef }: {
  caps: Capabilities
  route: string
  onSearch: () => void
  onToggleDrawer: () => void
  drawerOpen: boolean
  menuButtonRef: RefObject<HTMLButtonElement>
}) {
  return (
    <header className="topbar">
      <div className="topbar-inner">
        <button
          ref={menuButtonRef}
          className="menu-button"
          type="button"
          aria-label="Open navigation menu"
          aria-expanded={drawerOpen}
          aria-controls="primary-nav"
          onClick={onToggleDrawer}
        >
          <svg width="20" height="20" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true" focusable="false">
            <rect x="2" y="4" width="16" height="2" rx="1" /><rect x="2" y="9" width="16" height="2" rx="1" /><rect x="2" y="14" width="16" height="2" rx="1" />
          </svg>
          <span className="sr-only">Menu</span>
        </button>
        <Link to="/" className="topbar-brand" aria-label="Mitos home">
          <Mark size={22} glow />
          <strong>Mitos</strong>
        </Link>
        <div className="topbar-spacer" />
        <SearchTrigger onClick={onSearch} />
        <a className="topbar-help" href="/docs" target="_blank" rel="noreferrer">Help</a>
        <FeedbackButton caps={caps} route={route} />
        <ThemeToggle />
        <AccountMenu />
      </div>
    </header>
  )
}
