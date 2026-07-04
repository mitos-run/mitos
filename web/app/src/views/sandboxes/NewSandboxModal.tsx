// NewSandboxModal: create a sandbox from a template with a bounded vCPU/memory
// selection and an optional project. The vcpu/mem options are a conservative,
// static v1 set (1/2/4 vCPU; 1/2/4/8 GiB); the server re-validates them
// independently, so this list and the server's bounds must be kept in sync by
// hand (see internal/saas/console/sandbox_ops.go allowedVCPUs/allowedMemGiB).
//
// A11y: role="dialog" + aria-modal, labelled by the heading, Escape closes,
// the first field receives focus on open, matching InviteModal's pattern.
import { useEffect, useRef, useState } from 'react'
import { Button } from '@mitos/brand'
import { useCreateSandbox } from '../../data/sandboxes'
import { useTemplates } from '../../data/account'
import { useProjects } from '../../data/org'

const VCPU_OPTIONS = [1, 2, 4]
const MEM_GIB_OPTIONS = [1, 2, 4, 8]

export type NewSandboxModalProps = {
  onClose: () => void
  onCreated?: (id: string) => void
}

export function NewSandboxModal({ onClose, onCreated }: NewSandboxModalProps) {
  const { data: templates = [], isLoading: templatesLoading } = useTemplates()
  const { data: projects = [] } = useProjects()
  const createSandbox = useCreateSandbox()
  const [template, setTemplate] = useState('')
  const [vcpus, setVcpus] = useState(1)
  const [memGiB, setMemGiB] = useState(1)
  const [projectId, setProjectId] = useState('')
  const [error, setError] = useState<string | null>(null)
  const firstFieldRef = useRef<HTMLSelectElement>(null)

  useEffect(() => {
    firstFieldRef.current?.focus()
  }, [])

  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKeyDown)
    return () => document.removeEventListener('keydown', onKeyDown)
  }, [onClose])

  // Default the template select to the first template once templates load, so
  // a caller with only one template never has to open the select at all.
  useEffect(() => {
    if (!template && templates.length > 0) setTemplate(templates[0].name)
  }, [template, templates])

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!template || createSandbox.isPending) return
    setError(null)
    try {
      const created = await createSandbox.mutateAsync({
        template,
        vcpus,
        mem_gib: memGiB,
        project_id: projectId || undefined,
      })
      onCreated?.(created.id)
      onClose()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'failed to create sandbox')
    }
  }

  return (
    <div
      style={{
        position: 'fixed',
        inset: 0,
        background: 'color-mix(in srgb, black 60%, transparent)',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        zIndex: 100,
        padding: 'var(--space-4)',
      }}
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby="new-sandbox-modal-title"
        className="card"
        style={{ width: '100%', maxWidth: '480px', padding: 'var(--space-6)' }}
      >
        <h2 id="new-sandbox-modal-title" style={{ marginTop: 0, marginBottom: 'var(--space-2)' }}>
          New sandbox
        </h2>
        <p className="t-dim" style={{ marginTop: 0, marginBottom: 'var(--space-5)' }}>
          Start a fresh sandbox from a template.
        </p>

        {templatesLoading ? (
          <p className="t-dim">Loading templates...</p>
        ) : templates.length === 0 ? (
          <p className="t-dim">
            No templates are available yet. Create one with the CLI (<code>mitos template build</code>) before
            starting a sandbox here.
          </p>
        ) : (
          <form onSubmit={handleSubmit}>
            <div className="form-row" style={{ marginBottom: 'var(--space-4)' }}>
              <label htmlFor="new-sandbox-template">Template</label>
              <select
                id="new-sandbox-template"
                ref={firstFieldRef}
                value={template}
                onChange={(e) => setTemplate(e.target.value)}
              >
                {templates.map((t) => (
                  <option key={t.name} value={t.name}>
                    {t.name}
                  </option>
                ))}
              </select>
            </div>

            <div className="form-row" style={{ marginBottom: 'var(--space-4)' }}>
              <label htmlFor="new-sandbox-vcpus">Requested vCPUs</label>
              <select id="new-sandbox-vcpus" value={vcpus} onChange={(e) => setVcpus(Number(e.target.value))}>
                {VCPU_OPTIONS.map((v) => (
                  <option key={v} value={v}>
                    {v}
                  </option>
                ))}
              </select>
            </div>

            <div className="form-row" style={{ marginBottom: 'var(--space-2)' }}>
              <label htmlFor="new-sandbox-mem">Requested memory</label>
              <select id="new-sandbox-mem" value={memGiB} onChange={(e) => setMemGiB(Number(e.target.value))}>
                {MEM_GIB_OPTIONS.map((m) => (
                  <option key={m} value={m}>
                    {m} GiB
                  </option>
                ))}
              </select>
            </div>
            <p className="t-dim" style={{ fontSize: 'var(--step--1)', marginTop: 0, marginBottom: 'var(--space-4)' }}>
              Sizing is a request. Sandboxes currently run the template's resources; per-sandbox sizing is coming.
            </p>

            {projects.length > 0 && (
              <div className="form-row" style={{ marginBottom: 'var(--space-5)' }}>
                <label htmlFor="new-sandbox-project">Project (optional)</label>
                <select id="new-sandbox-project" value={projectId} onChange={(e) => setProjectId(e.target.value)}>
                  <option value="">Unassigned</option>
                  {projects.map((p) => (
                    <option key={p.id} value={p.id}>
                      {p.name}
                    </option>
                  ))}
                </select>
              </div>
            )}

            {error && (
              <p role="alert" style={{ color: 'var(--red, var(--magenta))', fontSize: 'var(--step--1)', marginBottom: 'var(--space-4)' }}>
                {error}
              </p>
            )}

            <div style={{ display: 'flex', gap: 'var(--space-3)', justifyContent: 'flex-end' }}>
              <button type="button" className="btn btn-ghost" onClick={onClose}>
                Cancel
              </button>
              <Button type="submit" variant="primary" disabled={!template || createSandbox.isPending}>
                {createSandbox.isPending ? 'Creating...' : 'Create sandbox'}
              </Button>
            </div>
          </form>
        )}

        {templates.length === 0 && !templatesLoading && (
          <div style={{ display: 'flex', justifyContent: 'flex-end', marginTop: 'var(--space-5)' }}>
            <button type="button" className="btn btn-ghost" onClick={onClose}>
              Close
            </button>
          </div>
        )}
      </div>
    </div>
  )
}
