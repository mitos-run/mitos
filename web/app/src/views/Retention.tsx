// Data and retention policy view. Lets org admins configure per-resource retention
// windows and a legal hold. Policy is stored by the BFF; enforcement is done by
// the garbage collector in the controller (issue #163). This view does NOT report
// deletion counts; it only stores and displays the policy.
import { useState, useEffect } from 'react'
import { useDataRetention, useSetDataRetention } from '../data/retention'
import { Skeleton } from '../ui/Skeleton'
import { useToast } from '../ui/Toast'
import type { DataRetentionPolicy } from '../api'
import { PageHeader } from '../ui/PageHeader'

function retentionLabel(days: number): string {
  if (days === 0) return 'Kept indefinitely'
  if (days === 1) return 'Removed after 1 day'
  return `Removed after ${days} days`
}

export function Retention() {
  const { data: policy, isLoading, isError } = useDataRetention()
  const setPolicy = useSetDataRetention()
  const { notify } = useToast()

  const [sandboxMetadataDays, setSandboxMetadataDays] = useState<number | ''>('')
  const [logsDays, setLogsDays] = useState<number | ''>('')
  const [usageDays, setUsageDays] = useState<number | ''>('')
  const [legalHold, setLegalHold] = useState<boolean>(false)
  const [initialized, setInitialized] = useState(false)

  // Sync form state from server once the policy loads (only the first time).
  useEffect(() => {
    if (policy && !initialized) {
      setSandboxMetadataDays(policy.sandbox_metadata_days)
      setLogsDays(policy.logs_days)
      setUsageDays(policy.usage_days)
      setLegalHold(policy.legal_hold)
      setInitialized(true)
    }
  }, [policy, initialized])

  const currentSandboxMetadataDays = sandboxMetadataDays === '' ? 0 : sandboxMetadataDays
  const currentLogsDays = logsDays === '' ? 0 : logsDays
  const currentUsageDays = usageDays === '' ? 0 : usageDays

  const handleSave = async () => {
    const updated: DataRetentionPolicy = {
      sandbox_metadata_days: currentSandboxMetadataDays,
      logs_days: currentLogsDays,
      usage_days: currentUsageDays,
      legal_hold: legalHold,
    }
    try {
      await setPolicy.mutateAsync(updated)
      notify('Retention policy saved')
    } catch {
      notify('Failed to save retention policy', 'error')
    }
  }

  if (isLoading) return <Skeleton rows={5} />
  if (isError) return <p className="t-dim">Failed to load retention policy. Please refresh.</p>

  function onSubmit(e: React.FormEvent) {
    e.preventDefault()
    void handleSave()
  }

  return (
    <div>
      <PageHeader title="Data and retention" lede="Configure per-resource retention windows for this org. The garbage collector in the controller enforces these settings on a scheduled basis. Setting a value to 0 means the data is kept forever. A legal hold pauses all automated deletion regardless of the configured windows." />

      <form onSubmit={onSubmit}>
        <section className="card" style={{ marginBottom: 'var(--space-6)' }}>
          <h2 style={{ marginBottom: 'var(--space-4)' }}>Retention windows</h2>
          <p className="t-dim" style={{ fontSize: 'var(--step--2)', marginBottom: 'var(--space-4)' }}>
            0 = keep forever. Values are in days.
          </p>

          <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)', maxWidth: 480 }}>
            <div>
              <label htmlFor="sandbox-metadata-days" style={{ display: 'block', marginBottom: 'var(--space-1)' }}>
                Sandbox metadata (days)
              </label>
              <input
                id="sandbox-metadata-days"
                type="number"
                min={0}
                value={sandboxMetadataDays}
                onChange={(e) => setSandboxMetadataDays(e.target.value === '' ? '' : Number(e.target.value))}
                style={{ width: '120px' }}
              />
            </div>

            <div>
              <label htmlFor="logs-days" style={{ display: 'block', marginBottom: 'var(--space-1)' }}>
                Logs (days)
              </label>
              <input
                id="logs-days"
                type="number"
                min={0}
                value={logsDays}
                onChange={(e) => setLogsDays(e.target.value === '' ? '' : Number(e.target.value))}
                style={{ width: '120px' }}
              />
            </div>

            <div>
              <label htmlFor="usage-days" style={{ display: 'block', marginBottom: 'var(--space-1)' }}>
                Usage (days)
              </label>
              <input
                id="usage-days"
                type="number"
                min={0}
                value={usageDays}
                onChange={(e) => setUsageDays(e.target.value === '' ? '' : Number(e.target.value))}
                style={{ width: '120px' }}
              />
            </div>
          </div>
        </section>

        <section className="card" style={{ marginBottom: 'var(--space-6)' }}>
          <h2 style={{ marginBottom: 'var(--space-3)' }}>Legal hold</h2>
          <p className="t-dim" style={{ fontSize: 'var(--step--1)', marginBottom: 'var(--space-4)' }}>
            Enabling legal hold pauses all automated deletion driven by the retention windows above.
            No data is removed while a legal hold is active, regardless of the configured periods.
          </p>
          <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-3)' }}>
            <input
              id="legal-hold"
              type="checkbox"
              checked={legalHold}
              onChange={(e) => setLegalHold(e.target.checked)}
            />
            <label htmlFor="legal-hold">
              Legal hold active
            </label>
          </div>
        </section>

        <section className="card" style={{ marginBottom: 'var(--space-6)' }}>
          <h2 style={{ marginBottom: 'var(--space-3)' }}>What gets deleted when</h2>
          <p className="t-dim" style={{ fontSize: 'var(--step--1)', marginBottom: 'var(--space-4)' }}>
            The garbage collector runs on a schedule in the controller. It applies the configured retention
            windows to each resource class. A legal hold prevents any deletion until it is lifted.
          </p>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)', fontSize: 'var(--step--1)' }}>
            <div>
              <strong>Sandbox metadata:</strong>{' '}
              <span className="t-dim">
                {retentionLabel(currentSandboxMetadataDays)}
                {legalHold ? ' (legal hold active, deletion paused)' : ''}
              </span>
            </div>
            <div>
              <strong>Logs:</strong>{' '}
              <span className="t-dim">
                {retentionLabel(currentLogsDays)}
                {legalHold ? ' (legal hold active, deletion paused)' : ''}
              </span>
            </div>
            <div>
              <strong>Usage records:</strong>{' '}
              <span className="t-dim">
                {retentionLabel(currentUsageDays)}
                {legalHold ? ' (legal hold active, deletion paused)' : ''}
              </span>
            </div>
          </div>
        </section>

        <button
          type="submit"
          className="btn btn-primary"
          disabled={setPolicy.isPending}
        >
          {setPolicy.isPending ? 'Saving...' : 'Save'}
        </button>
      </form>
    </div>
  )
}
