// Account settings view: Profile, Security (sessions), and Appearance tabs.
// Profile: editable display name / timezone / locale form + read-only email + memberships.
// Security: sessions table with per-row revoke and a sign-out-everywhere button.
// Appearance: reduced-motion toggle and density select applied immediately to the document.
import { useState } from 'react'
import { Tabs, type TabDef } from '../ui/Tabs'
import { Skeleton } from '../ui/Skeleton'
import { EmptyState } from '../ui/EmptyState'
import { useToast } from '../ui/Toast'
import { useAccount, useUpdateAccount, useSessions, useRevokeSession, useRevokeAllSessions } from '../data/account-settings'
import { getAppearance, setAppearance } from '../appearance'

const TABS: TabDef[] = [
  { key: 'profile', label: 'Profile' },
  { key: 'security', label: 'Security' },
  { key: 'appearance', label: 'Appearance' },
]

// --- Profile tab ---

function ProfileTab() {
  const { data: account, isLoading, isError } = useAccount()
  const updateAccount = useUpdateAccount()
  const { notify } = useToast()

  const [displayName, setDisplayName] = useState<string | undefined>(undefined)
  const [timezone, setTimezone] = useState<string | undefined>(undefined)
  const [locale, setLocale] = useState<string | undefined>(undefined)

  // Use local state overrides if the user has started editing; fall back to fetched values.
  const currentDisplayName = displayName ?? account?.display_name ?? ''
  const currentTimezone = timezone ?? account?.timezone ?? ''
  const currentLocale = locale ?? account?.locale ?? ''

  function onSave(e: React.FormEvent) {
    e.preventDefault()
    updateAccount.mutate(
      { display_name: currentDisplayName, timezone: currentTimezone, locale: currentLocale },
      {
        onSuccess: () => {
          notify('Profile saved', 'ok')
          // Clear local overrides so we reflect the server response.
          setDisplayName(undefined)
          setTimezone(undefined)
          setLocale(undefined)
        },
        onError: () => notify('Failed to save profile', 'error'),
      },
    )
  }

  if (isLoading) return <Skeleton rows={4} />
  if (isError) return <p className="t-dim">Failed to load account. Please refresh.</p>
  if (!account) return null

  return (
    <section>
      <h3>Profile</h3>

      <div style={{ marginBottom: 'var(--space-5)' }}>
        <p className="t-dim" style={{ fontSize: 'var(--step--1)' }}>
          <strong>Email:</strong> {account.email}
        </p>
      </div>

      <form onSubmit={onSave} style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)', maxWidth: 480 }}>
        <div>
          <label htmlFor="display-name" style={{ display: 'block', marginBottom: 'var(--space-1)' }}>
            Display name
          </label>
          <input
            id="display-name"
            type="text"
            value={currentDisplayName}
            onChange={(e) => setDisplayName(e.target.value)}
            className="input"
          />
        </div>

        <div>
          <label htmlFor="timezone" style={{ display: 'block', marginBottom: 'var(--space-1)' }}>
            Timezone
          </label>
          <input
            id="timezone"
            type="text"
            value={currentTimezone}
            onChange={(e) => setTimezone(e.target.value)}
            className="input"
          />
        </div>

        <div>
          <label htmlFor="locale" style={{ display: 'block', marginBottom: 'var(--space-1)' }}>
            Locale
          </label>
          <input
            id="locale"
            type="text"
            value={currentLocale}
            onChange={(e) => setLocale(e.target.value)}
            className="input"
          />
        </div>

        <div>
          <button type="submit" disabled={updateAccount.isPending}>
            {updateAccount.isPending ? 'Saving...' : 'Save'}
          </button>
        </div>
      </form>

      {account.memberships.length > 0 && (
        <div style={{ marginTop: 'var(--space-7)' }}>
          <h4 style={{ marginBottom: 'var(--space-4)' }}>Memberships</h4>
          <div style={{ overflowX: 'auto' }}>
            <table className="tbl" aria-label="Memberships">
              <thead>
                <tr>
                  <th scope="col">Org</th>
                  <th scope="col">Role</th>
                  <th scope="col">Joined</th>
                </tr>
              </thead>
              <tbody>
                {account.memberships.map((m) => (
                  <tr key={`${m.account_id}-${m.org_id}`}>
                    <td>{m.org_id}</td>
                    <td>
                      <span className={`role-badge role-${m.role}`}>{m.role}</span>
                    </td>
                    <td className="t-dim">{new Date(m.created_at).toLocaleDateString()}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </section>
  )
}

// --- Security tab ---

function SecurityTab() {
  const { data: sessions = [], isLoading, isError } = useSessions()
  const revokeSession = useRevokeSession()
  const revokeAll = useRevokeAllSessions()
  const { notify } = useToast()

  function onRevoke(id: string) {
    revokeSession.mutate(id, {
      onSuccess: () => notify('Session revoked', 'ok'),
      onError: () => notify('Failed to revoke session', 'error'),
    })
  }

  function onSignOutEverywhere() {
    revokeAll.mutate(undefined, {
      onSuccess: () => notify('Signed out everywhere', 'ok'),
      onError: () => notify('Failed to sign out everywhere', 'error'),
    })
  }

  return (
    <section>
      <h3>Security</h3>
      <p className="t-dim" style={{ fontSize: 'var(--step--1)', marginBottom: 'var(--space-5)' }}>
        Active sessions for your account. Revoke any session you do not recognise.
      </p>

      {isLoading ? (
        <Skeleton rows={2} />
      ) : isError ? (
        <p className="t-dim">Failed to load sessions. Please refresh.</p>
      ) : sessions.length === 0 ? (
        <EmptyState title="No sessions" body="No active sessions found." />
      ) : (
        <div style={{ overflowX: 'auto' }}>
          <table className="tbl" aria-label="Sessions">
            <thead>
              <tr>
                <th scope="col">Label</th>
                <th scope="col">Created</th>
                <th scope="col">Status</th>
                <th scope="col"></th>
              </tr>
            </thead>
            <tbody>
              {sessions.map((s) => (
                <tr key={s.id}>
                  <td>{s.label}</td>
                  <td className="t-dim">{new Date(s.created_at).toLocaleDateString()}</td>
                  <td>{s.current ? <span className="t-dim">Current</span> : null}</td>
                  <td>
                    {!s.current && (
                      <button onClick={() => onRevoke(s.id)} disabled={revokeSession.isPending}>
                        Revoke
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <div style={{ marginTop: 'var(--space-6)' }}>
        <button onClick={onSignOutEverywhere} disabled={revokeAll.isPending}>
          Sign out everywhere
        </button>
      </div>
    </section>
  )
}

// --- Appearance tab ---

function AppearanceTab() {
  const [prefs, setPrefs] = useState(() => getAppearance())

  function onReducedMotionChange(e: React.ChangeEvent<HTMLInputElement>) {
    const next = { ...prefs, reducedMotion: e.target.checked }
    setPrefs(next)
    setAppearance(next)
  }

  function onDensityChange(e: React.ChangeEvent<HTMLSelectElement>) {
    const next = { ...prefs, density: e.target.value as 'comfortable' | 'compact' }
    setPrefs(next)
    setAppearance(next)
  }

  return (
    <section>
      <h3>Appearance</h3>
      <p className="t-dim" style={{ fontSize: 'var(--step--1)', marginBottom: 'var(--space-5)' }}>
        Changes apply immediately and persist across sessions.
      </p>

      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)', maxWidth: 400 }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-3)' }}>
          <input
            id="reduced-motion"
            type="checkbox"
            checked={prefs.reducedMotion}
            onChange={onReducedMotionChange}
          />
          <label htmlFor="reduced-motion">Reduced motion</label>
        </div>

        <div>
          <label htmlFor="density" style={{ display: 'block', marginBottom: 'var(--space-1)' }}>
            Density
          </label>
          <select id="density" value={prefs.density} onChange={onDensityChange} aria-label="Density">
            <option value="comfortable">Comfortable</option>
            <option value="compact">Compact</option>
          </select>
        </div>
      </div>
    </section>
  )
}

// --- Main Settings view ---

export function Settings() {
  const [activeTab, setActiveTab] = useState('profile')

  return (
    <div>
      <h2>Settings</h2>
      <Tabs tabs={TABS} active={activeTab} onChange={setActiveTab} ariaLabel="Settings" />
      <div style={{ marginTop: 'var(--space-6)' }}>
        {activeTab === 'profile' && <ProfileTab />}
        {activeTab === 'security' && <SecurityTab />}
        {activeTab === 'appearance' && <AppearanceTab />}
      </div>
    </div>
  )
}
