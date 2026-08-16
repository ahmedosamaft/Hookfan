import { useCallback, useEffect, useState } from 'react'
import { NavLink, Route, Routes, useNavigate } from 'react-router-dom'
import { api, clearToken, getToken, setToken } from './lib/api'
import { useStream } from './lib/useStream'
import { Button, Field, Input } from './components/ui'
import Dashboard from './screens/Dashboard'
import Listeners from './screens/Listeners'
import Services from './screens/Services'
import Subscriptions from './screens/Subscriptions'
import Events from './screens/Events'

const NAV = [
  { to: '/', label: 'Dashboard', end: true },
  { to: '/events', label: 'Events' },
  { to: '/listeners', label: 'Listeners' },
  { to: '/services', label: 'Services' },
  { to: '/subscriptions', label: 'Subscriptions' },
]

export default function App() {
  const [authed, setAuthed] = useState(() => getToken() !== null)

  if (!authed) return <Login onAuthed={() => setAuthed(true)} />
  return <Shell onSignOut={() => { clearToken(); setAuthed(false) }} />
}

function Login({ onAuthed }: { onAuthed: () => void }) {
  const [token, setTokenValue] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError(null)
    setToken(token.trim())
    try {
      // Any authenticated endpoint proves the token before we commit to it.
      await api.listListeners()
      onAuthed()
    } catch (err) {
      clearToken()
      setError(err instanceof Error ? err.message : 'Could not authenticate')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="flex min-h-full items-center justify-center p-6">
      <form onSubmit={submit} className="w-full max-w-sm rounded-lg border border-ink-700 bg-ink-900 p-5">
        <h1 className="mb-1 font-mono text-lg font-semibold text-ink-100">hookfan</h1>
        <p className="mb-4 text-xs text-ink-400">Webhook fan-out gateway</p>

        <Field label="Admin token" hint="The ADMIN_TOKEN from the API environment. Held for this browser session only.">
          <Input
            type="password"
            value={token}
            onChange={e => setTokenValue(e.target.value)}
            placeholder="paste token"
            autoFocus
          />
        </Field>

        {error && (
          <p className="mt-3 rounded border border-bad-500/40 bg-bad-500/10 px-2 py-1.5 text-xs text-bad-400">
            {error}
          </p>
        )}

        <Button type="submit" variant="primary" className="mt-4 w-full justify-center" disabled={!token.trim() || busy}>
          {busy ? 'Checking…' : 'Sign in'}
        </Button>
      </form>
    </div>
  )
}

function Shell({ onSignOut }: { onSignOut: () => void }) {
  const navigate = useNavigate()
  const [liveCount, setLiveCount] = useState(0)

  const onStreamEvent = useCallback(() => setLiveCount(n => n + 1), [])
  const { connected } = useStream(onStreamEvent)

  // "/" focuses event search, per the spec. Ignored while typing in a field.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const target = e.target as HTMLElement | null
      const typing = target && /^(INPUT|TEXTAREA|SELECT)$/.test(target.tagName)
      if (e.key === '/' && !typing) {
        e.preventDefault()
        navigate('/events')
        // The Events screen registers this input; wait a tick for it to mount.
        setTimeout(() => document.getElementById('event-search')?.focus(), 30)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [navigate])

  return (
    <div className="flex min-h-full flex-col">
      <header className="sticky top-0 z-30 flex items-center gap-4 border-b border-ink-800 bg-ink-900/95 px-4 py-2 backdrop-blur">
        <span className="font-mono text-sm font-semibold text-ink-100">hookfan</span>

        <nav className="flex gap-0.5">
          {NAV.map(item => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.end}
              className={({ isActive }) =>
                `rounded px-2.5 py-1 text-xs transition-colors ${
                  isActive ? 'bg-ink-800 text-ink-100' : 'text-ink-400 hover:bg-ink-850 hover:text-ink-200'
                }`
              }
            >
              {item.label}
            </NavLink>
          ))}
        </nav>

        <div className="ml-auto flex items-center gap-3">
          <span className="flex items-center gap-1.5 text-[11px] text-ink-400" title={connected ? 'Live updates connected' : 'Reconnecting…'}>
            <span className={`h-1.5 w-1.5 rounded-full ${connected ? 'bg-ok-400' : 'bg-warn-400'}`} />
            {connected ? 'live' : 'offline'}
            {liveCount > 0 && <span className="font-mono text-ink-300">· {liveCount}</span>}
          </span>
          <Button variant="ghost" size="xs" onClick={onSignOut}>Sign out</Button>
        </div>
      </header>

      <main className="flex-1">
        <Routes>
          <Route path="/" element={<Dashboard />} />
          <Route path="/events" element={<Events />} />
          <Route path="/listeners" element={<Listeners />} />
          <Route path="/services" element={<Services />} />
          <Route path="/subscriptions" element={<Subscriptions />} />
        </Routes>
      </main>
    </div>
  )
}
