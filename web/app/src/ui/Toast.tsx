// Minimal toast system: a provider holds a queue, useToast().notify pushes a
// message, each auto-dismisses after 3s. Used for optimistic-mutation feedback
// in later phases (create key, terminate sandbox).
import { createContext, useContext, useCallback, useState, type ReactNode } from 'react'

type Toast = { id: number; msg: string; kind: 'ok' | 'error' }
type ToastApi = { notify: (msg: string, kind?: 'ok' | 'error') => void }

const ToastContext = createContext<ToastApi | null>(null)

let nextId = 1

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])
  const notify = useCallback((msg: string, kind: 'ok' | 'error' = 'ok') => {
    const id = nextId++
    setToasts((t) => [...t, { id, msg, kind }])
    setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), 3000)
  }, [])
  return (
    <ToastContext.Provider value={{ notify }}>
      {children}
      <div className="toast-stack" style={{ position: 'fixed', bottom: 'var(--space-5)', right: 'var(--space-5)' }}>
        {toasts.map((t) => (
          <div key={t.id} role="status" className={`toast toast-${t.kind}`}>{t.msg}</div>
        ))}
      </div>
    </ToastContext.Provider>
  )
}

export function useToast(): ToastApi {
  const ctx = useContext(ToastContext)
  if (!ctx) throw new Error('useToast must be used inside a ToastProvider')
  return ctx
}
