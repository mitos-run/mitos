// Org secrets (#275). Write-only: the value is sent on create and never shown
// again — the list renders metadata + fingerprint only.
import { useEffect, useState } from 'react'
import { Button, Card } from '@mitos/brand'
import { api, type SecretView } from '../api'

export function Secrets() {
  const [rows, setRows] = useState<SecretView[]>([])
  const [name, setName] = useState('')
  const [value, setValue] = useState('')
  const [err, setErr] = useState<string>()

  const reload = () => api.secrets().then(setRows).catch((e) => setErr(String(e)))
  useEffect(() => {
    reload()
  }, [])

  const create = async () => {
    setErr(undefined)
    try {
      await api.createSecret(name, value)
      setName('')
      setValue('')
      await reload()
    } catch (e) {
      setErr(String(e))
    }
  }

  return (
    <div>
      <h2>Secrets</h2>
      <p className="t-dim" style={{ fontSize: 'var(--step--1)' }}>
        Write-only. Values are encrypted server-side and injected into sandboxes; they are never shown again — rotate, don't read.
      </p>
      <Card style={{ marginBottom: 'var(--space-5)' }}>
        <div style={{ display: 'flex', gap: 'var(--space-2)', flexWrap: 'wrap' }}>
          <input className="mono" placeholder="NAME (e.g. OPENAI_API_KEY)" value={name} onChange={(e) => setName(e.target.value)} />
          <input className="mono" type="password" placeholder="value" value={value} onChange={(e) => setValue(e.target.value)} />
          <Button onClick={create} disabled={!name || !value}>Store</Button>
        </div>
        {err && <div className="t-dim" style={{ marginTop: 'var(--space-2)' }}>{err}</div>}
      </Card>
      <table className="tbl">
        <thead>
          <tr><th>Name</th><th>Provider</th><th>Version</th><th>Fingerprint</th><th /></tr>
        </thead>
        <tbody>
          {rows.map((s) => (
            <tr key={s.name}>
              <td className="mono">{s.name}</td>
              <td>{s.provider}</td>
              <td>{s.version}</td>
              <td className="mono t-dim">{s.fingerprint}</td>
              <td><Button variant="ghost" onClick={() => api.deleteSecret(s.name).then(reload)}>Delete</Button></td>
            </tr>
          ))}
          {rows.length === 0 && <tr><td colSpan={5} className="t-dim">no secrets yet</td></tr>}
        </tbody>
      </table>
    </div>
  )
}
