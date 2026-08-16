import { useCallback, useEffect, useState } from 'react'
import { api, ApiError } from '../lib/api'
import type { Service, ServiceWithToken, VerifyResult } from '../lib/types'
import {
  Banner, Button, CopyButton, EmptyState, Field, Input, Modal, Mono, Pill, Select, Spinner,
} from '../components/ui'
import { SNIPPET_LANGUAGES, snippet, type SnippetLanguage } from './snippets'

export default function Services() {
  const [services, setServices] = useState<Service[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [creating, setCreating] = useState(false)
  const [issued, setIssued] = useState<ServiceWithToken | null>(null)
  const [verifying, setVerifying] = useState<string | null>(null)
  const [results, setResults] = useState<Record<string, VerifyResult>>({})

  const load = useCallback(async () => {
    try {
      setServices(await api.listServices())
      setError(null)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not load services')
    }
  }, [])
  useEffect(() => { void load() }, [load])

  async function verify(s: Service) {
    setVerifying(s.id)
    try {
      // Succeeds or fails, the response carries the specific reason.
      const { result } = await api.verifyService(s.id)
      setResults(r => ({ ...r, [s.id]: result }))
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Verification failed')
    } finally {
      setVerifying(null)
      await load()
    }
  }

  async function rotate(s: Service) {
    if (!confirm(`Rotate the link token for "${s.name}"?\n\nThe current token stops working immediately and the service returns to pending until you re-verify.`)) return
    try {
      setIssued(await api.rotateToken(s.id))
      await load()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not rotate token')
    }
  }

  async function reenable(s: Service) {
    try {
      await api.updateService(s.id, { reset_breaker: true })
      await load()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not re-enable service')
    }
  }

  async function remove(s: Service) {
    if (!confirm(`Delete service "${s.name}"?\n\nIts subscriptions and delivery history are deleted too.`)) return
    try {
      await api.deleteService(s.id)
      await load()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not delete service')
    }
  }

  const disabled = services?.filter(s => s.status === 'disabled') ?? []

  return (
    <div className="space-y-3 p-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-sm font-semibold text-ink-100">Services</h1>
          <p className="text-xs text-ink-400">Backends that receive forwarded events.</p>
        </div>
        <Button variant="primary" onClick={() => setCreating(true)}>New service</Button>
      </div>

      {error && <Banner>{error}</Banner>}
      {disabled.length > 0 && (
        <Banner>
          <strong>{disabled.length} service{disabled.length > 1 ? 's' : ''} disabled by the circuit breaker</strong>{' '}
          after repeated failures. No events are being delivered to them until re-enabled.
        </Banner>
      )}

      <div className="overflow-hidden rounded-lg border border-ink-800">
        <table className="w-full text-left">
          <thead className="bg-ink-850 text-[11px] uppercase tracking-wide text-ink-400">
            <tr>
              <th className="px-3 py-1.5 font-medium">Name</th>
              <th className="px-3 py-1.5 font-medium">URL</th>
              <th className="px-3 py-1.5 font-medium">Status</th>
              <th className="px-3 py-1.5 font-medium">Retries</th>
              <th className="px-3 py-1.5 font-medium"></th>
            </tr>
          </thead>
          <tbody className="divide-y divide-ink-800 bg-ink-900">
            {services === null && (
              <tr><td colSpan={5} className="px-3 py-6 text-center"><Spinner /></td></tr>
            )}
            {services?.length === 0 && (
              <tr><td colSpan={5}>
                <EmptyState title="No services yet"
                  hint="Add a backend, copy its link token, then click Verify." />
              </td></tr>
            )}
            {services?.map(s => {
              const result = results[s.id]
              return (
                <tr key={s.id} className="align-top hover:bg-ink-850">
                  <td className="px-3 py-1.5">
                    <div className="text-xs text-ink-100">{s.name}</div>
                    <Mono className="text-ink-400">{s.id}</Mono>
                  </td>
                  <td className="px-3 py-1.5">
                    <Mono className="text-ink-300">{s.method} {s.url}</Mono>
                  </td>
                  <td className="px-3 py-1.5">
                    <Pill status={s.status} />
                    {/* The exact failure reason, inline — a vague error here
                        wastes hours. */}
                    {result && !result.ok && (
                      <div className="mt-1 max-w-md rounded border border-bad-500/30 bg-bad-500/5 px-1.5 py-1">
                        {result.kind && (
                          <Mono className="block text-bad-400">{result.kind}</Mono>
                        )}
                        <span className="block text-[11px] text-ink-300">{result.message}</span>
                        {result.echoed && (
                          <span className="mt-0.5 block font-mono text-[11px] text-ink-400">
                            got: {result.echoed}
                          </span>
                        )}
                      </div>
                    )}
                    {result?.ok && (
                      <Mono className="mt-1 block text-ok-400">verified in {result.latency_ms}ms</Mono>
                    )}
                    {!result && s.last_verify_error && s.status === 'failed' && (
                      <div className="mt-1 max-w-md text-[11px] text-bad-400">{s.last_verify_error}</div>
                    )}
                    {s.disabled_reason && (
                      <div className="mt-1 text-[11px] text-bad-400">{s.disabled_reason}</div>
                    )}
                  </td>
                  <td className="px-3 py-1.5">
                    <Mono className="text-ink-400">
                      {s.max_attempts}× · {s.timeout_ms}ms
                      {s.rate_limit_rps > 0 && ` · ${s.rate_limit_rps}/s`}
                    </Mono>
                  </td>
                  <td className="px-3 py-1.5 text-right">
                    <div className="flex justify-end gap-1">
                      <Button size="xs" onClick={() => verify(s)} disabled={verifying === s.id}>
                        {verifying === s.id ? <Spinner /> : 'Verify'}
                      </Button>
                      {s.status === 'disabled' && (
                        <Button size="xs" onClick={() => reenable(s)}>Re-enable</Button>
                      )}
                      <Button size="xs" onClick={() => rotate(s)}>Rotate</Button>
                      <Button size="xs" variant="danger" onClick={() => remove(s)}>Delete</Button>
                    </div>
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>

      <CreateService
        open={creating}
        onClose={() => setCreating(false)}
        onCreated={async r => { setCreating(false); setIssued(r); await load() }}
      />
      <TokenModal issued={issued} onClose={() => setIssued(null)} />
    </div>
  )
}

function CreateService({ open, onClose, onCreated }: {
  open: boolean; onClose: () => void; onCreated: (r: ServiceWithToken) => void
}) {
  const [form, setForm] = useState({ name: '', url: '', method: 'POST' })
  const [errors, setErrors] = useState<string[]>([])
  const [busy, setBusy] = useState(false)

  const set = (k: keyof typeof form) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setForm(f => ({ ...f, [k]: e.target.value }))

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setErrors([])
    try {
      const created = await api.createService(form)
      setForm({ name: '', url: '', method: 'POST' })
      onCreated(created)
    } catch (err) {
      if (err instanceof ApiError) setErrors(err.errors ?? [err.message])
      else setErrors([String(err)])
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal open={open} onClose={onClose} title="New service">
      <form onSubmit={submit} className="space-y-3">
        <Field label="Name">
          <Input value={form.name} onChange={set('name')} placeholder="orders-api" autoFocus />
        </Field>
        <Field label="URL" hint="Where forwarded events are POSTed.">
          <Input value={form.url} onChange={set('url')} placeholder="https://orders.internal/hooks/meta" />
        </Field>
        <Field label="Method">
          <Select value={form.method} onChange={set('method')} className="w-full">
            <option>POST</option><option>PUT</option><option>PATCH</option>
          </Select>
        </Field>

        {errors.length > 0 && (
          <Banner>
            <ul className="list-inside list-disc space-y-0.5">
              {errors.map(e => <li key={e}>{e}</li>)}
            </ul>
          </Banner>
        )}

        <div className="flex justify-end gap-2 pt-1">
          <Button type="button" onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="primary" disabled={busy}>
            {busy ? 'Creating…' : 'Create service'}
          </Button>
        </div>
      </form>
    </Modal>
  )
}

/** The token is shown exactly once, alongside a working receiver for the
 * language the operator is using. */
function TokenModal({ issued, onClose }: { issued: ServiceWithToken | null; onClose: () => void }) {
  const [lang, setLang] = useState<SnippetLanguage>('Go')
  if (!issued) return null

  return (
    <Modal open onClose={onClose} title={`Link token for ${issued.service.name}`} wide>
      <div className="space-y-4">
        <Banner tone="warn">{issued.warning}</Banner>

        <div>
          <p className="mb-1 text-[11px] uppercase tracking-wide text-ink-400">Link token</p>
          <div className="flex items-center gap-1.5">
            <code className="flex-1 overflow-x-auto rounded border border-ink-700 bg-ink-950 px-2 py-1.5 font-mono text-xs text-ok-400">
              {issued.link_token}
            </code>
            <CopyButton value={issued.link_token} />
          </div>
        </div>

        <div>
          <div className="mb-1.5 flex items-center justify-between">
            <p className="text-[11px] uppercase tracking-wide text-ink-400">Receiver</p>
            <div className="flex gap-0.5">
              {SNIPPET_LANGUAGES.map(l => (
                <button
                  key={l}
                  onClick={() => setLang(l)}
                  className={`rounded px-2 py-0.5 text-[11px] ${
                    lang === l ? 'bg-ink-700 text-ink-100' : 'text-ink-400 hover:text-ink-200'
                  }`}
                >
                  {l}
                </button>
              ))}
            </div>
          </div>
          <div className="relative">
            <pre className="max-h-80 overflow-auto rounded border border-ink-700 bg-ink-950 p-2.5 font-mono text-[11px] leading-relaxed text-ink-200">
              {snippet(lang, issued.service.url)}
            </pre>
            <div className="absolute right-2 top-2">
              <CopyButton value={snippet(lang, issued.service.url)} />
            </div>
          </div>
        </div>

        <p className="text-[11px] text-ink-400">
          Configure the token on your backend, then click <strong>Verify</strong>.
          A service only receives events once it is verified.
        </p>
      </div>
    </Modal>
  )
}
