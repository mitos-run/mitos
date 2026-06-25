// The account menu: caller identity, a link to account settings, and sign out.
// A real menu button: aria-haspopup, aria-expanded, Escape and outside-click
// close, focus returns to the trigger.
import { useEffect, useRef, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useAccount, useSignOut } from '../data/account-settings'

export function AccountMenu() {
  const { data: account } = useAccount()
  const signOut = useSignOut()
  const [open, setOpen] = useState(false)
  const btnRef = useRef<HTMLButtonElement>(null)
  const popRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!open) return
    function onDocClick(e: MouseEvent) {
      if (!popRef.current?.contains(e.target as Node) && !btnRef.current?.contains(e.target as Node)) setOpen(false)
    }
    function onKey(e: KeyboardEvent) { if (e.key === 'Escape') { setOpen(false); btnRef.current?.focus() } }
    document.addEventListener('mousedown', onDocClick)
    document.addEventListener('keydown', onKey)
    return () => { document.removeEventListener('mousedown', onDocClick); document.removeEventListener('keydown', onKey) }
  }, [open])

  const initial = (account?.display_name || account?.email || '?').trim().charAt(0).toUpperCase()

  return (
    <div className="account-menu">
      <button
        ref={btnRef}
        type="button"
        className="account-avatar"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label="Account menu"
        onClick={() => setOpen((v) => !v)}
      >
        {initial}
      </button>
      {open && (
        <div ref={popRef} role="menu" className="account-pop">
          <div className="account-id">
            <div className="account-name">{account?.display_name}</div>
            <div className="t-dim">{account?.email}</div>
          </div>
          <Link role="menuitem" to="/settings" className="account-item" onClick={() => setOpen(false)}>Account settings</Link>
          <button role="menuitem" type="button" className="account-item account-signout" disabled={signOut.isPending} onClick={() => signOut.mutate()}>Sign out</button>
        </div>
      )}
    </div>
  )
}
