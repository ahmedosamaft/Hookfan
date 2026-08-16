import { useCallback, useEffect, useState } from 'react'
import { api, ApiError } from '../lib/api'
import type { FilterType, Listener, Service, Subscription } from '../lib/types'
import {
  Banner, Button, EmptyState, Field, Modal, Mono, Pill, Select, Spinner,
} from '../components/ui'

export default function Subscriptions() {
  const [subs, setSubs] = useState<Subscription[] | null>(null)
  const [listeners, setListeners] = useState<Listener[]>([])
  const [services, setServices] = useState<Service[]>([])
  const [error, setError] = useState<string | null>(null)
  const [creating, setCreating] = useState(false)

  const load = useCallback(async () => {
    try {
      const [s, l, svc] = await Promise.all([
        api.listSubscriptions(), api.listListeners(), api.listServices(),
      ])
      setSubs(s); setListeners(l); setServices(svc)
      setError(null)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not load subscriptions')
    }
  }, [])
  useEffect(() => { void load() }, [load])

  const listenerName = (id: number) => listeners.find(l => l.id === id)?.slug ?? `#${id}`
  const service = (id: string) => services.find(s => s.id === id)

  async function toggle(s: Subscription) {
    try {
      await api.updateSubscription(s.id, { enabled: !s.enabled })
      await load()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not update subscription')
    }
  }

  async function remove(s: Subscription) {
    if (!confirm('Delete this subscription? Events will stop reaching that service.')) return
    try {
      await api.deleteSubscription(s.id)
      await load()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not delete subscription')
    }
  }

  return (
    <div className="space-y-3 p-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-sm font-semibold text-ink-100">Subscriptions</h1>
          <p className="text-xs text-ink-400">
            Which services receive which events. A service matching several
            subscriptions still receives exactly one delivery.
          </p>
        </div>
        <Button variant="primary" onClick={() => setCreating(true)}
                disabled={listeners.length === 0 || services.length === 0}>
          New subscription
        </Button>
      </div>

      {error && <Banner>{error}</Banner>}
      {(listeners.length === 0 || services.length === 0) && (
        <Banner tone="info">
          A subscription joins a listener to a service — create at least one of each first.
        </Banner>
      )}

      <div className="overflow-hidden rounded-lg border border-ink-800">
        <table className="w-full text-left">
          <thead className="bg-ink-850 text-[11px] uppercase tracking-wide text-ink-400">
            <tr>
              <th className="px-3 py-1.5 font-medium">Listener</th>
              <th className="px-3 py-1.5 font-medium">Service</th>
              <th className="px-3 py-1.5 font-medium">Filter</th>
              <th className="px-3 py-1.5 font-medium">Matches</th>
              <th className="px-3 py-1.5 font-medium"></th>
            </tr>
          </thead>
          <tbody className="divide-y divide-ink-800 bg-ink-900">
            {subs === null && (
              <tr><td colSpan={5} className="px-3 py-6 text-center"><Spinner /></td></tr>
            )}
            {subs?.length === 0 && (
              <tr><td colSpan={5}>
                <EmptyState title="No subscriptions yet"
                  hint="Without one, received events are stored but never forwarded." />
              </td></tr>
            )}
            {subs?.map(s => {
              const svc = service(s.service_id)
              return (
                <tr key={s.id} className="hover:bg-ink-850">
                  <td className="px-3 py-1.5"><Mono className="text-ink-200">{listenerName(s.listener_id)}</Mono></td>
                  <td className="px-3 py-1.5">
                    <div className="flex items-center gap-2">
                      <span className="text-xs text-ink-100">{svc?.name ?? s.service_id}</span>
                      {svc && svc.status !== 'verified' && <Pill status={svc.status} />}
                    </div>
                  </td>
                  <td className="px-3 py-1.5">
                    <div className="flex items-center gap-1.5">
                      <Mono className="text-ink-300">{s.filter_type}</Mono>
                      {s.is_default && <Pill status="pending">default</Pill>}
                      {!s.enabled && <Pill status="disabled">off</Pill>}
                    </div>
                  </td>
                  <td className="px-3 py-1.5">
                    {s.filter_type === 'routing_key_in' && (
                      <div className="flex flex-wrap gap-1">
                        {s.routing_keys.map(k => (
                          <span key={k} className="rounded bg-ink-800 px-1.5 py-0.5 font-mono text-[11px] text-ink-200">
                            {k}
                          </span>
                        ))}
                      </div>
                    )}
                    {s.filter_type === 'jsonpath_match' && (
                      <Mono className="text-ink-400">
                        {s.filter_expr.map(c => `${c.path} ${c.op} ${JSON.stringify(c.value ?? '')}`).join(' AND ')}
                      </Mono>
                    )}
                    {s.filter_type === 'all' && <Mono className="text-ink-400">every event</Mono>}
                  </td>
                  <td className="px-3 py-1.5 text-right">
                    <div className="flex justify-end gap-1">
                      <Button size="xs" onClick={() => toggle(s)}>{s.enabled ? 'Disable' : 'Enable'}</Button>
                      <Button size="xs" variant="danger" onClick={() => remove(s)}>Delete</Button>
                    </div>
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>

      <CreateSubscription
        open={creating}
        listeners={listeners}
        services={services}
        onClose={() => setCreating(false)}
        onCreated={async () => { setCreating(false); await load() }}
      />
    </div>
  )
}

function CreateSubscription({ open, listeners, services, onClose, onCreated }: {
  open: boolean; listeners: Listener[]; services: Service[]
  onClose: () => void; onCreated: () => void
}) {
  const [listenerId, setListenerId] = useState('')
  const [serviceId, setServiceId] = useState('')
  const [filterType, setFilterType] = useState<FilterType>('all')
  const [keys, setKeys] = useState<string[]>([])
  const [expr, setExpr] = useState('[{"path": "object", "op": "eq", "value": "whatsapp_business_account"}]')
  const [isDefault, setIsDefault] = useState(false)
  const [errors, setErrors] = useState<string[]>([])
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (open) {
      setListenerId(listeners[0] ? String(listeners[0].id) : '')
      setServiceId(services[0]?.id ?? '')
    }
  }, [open, listeners, services])

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setErrors([])
    try {
      const body: Record<string, unknown> = {
        listener_id: Number(listenerId),
        service_id: serviceId,
        filter_type: filterType,
        is_default: isDefault,
      }
      if (filterType === 'routing_key_in') body.routing_keys = keys
      if (filterType === 'jsonpath_match') {
        try {
          body.filter_expr = JSON.parse(expr)
        } catch {
          setErrors(['filter_expr must be valid JSON'])
          setBusy(false)
          return
        }
      }
      await api.createSubscription(body)
      setKeys([])
      onCreated()
    } catch (err) {
      if (err instanceof ApiError) setErrors(err.errors ?? [err.message])
      else setErrors([String(err)])
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal open={open} onClose={onClose} title="New subscription">
      <form onSubmit={submit} className="space-y-3">
        <Field label="Listener">
          <Select value={listenerId} onChange={e => setListenerId(e.target.value)} className="w-full">
            {listeners.map(l => <option key={l.id} value={l.id}>{l.slug} — {l.name}</option>)}
          </Select>
        </Field>
        <Field label="Service">
          <Select value={serviceId} onChange={e => setServiceId(e.target.value)} className="w-full">
            {services.map(s => (
              <option key={s.id} value={s.id}>
                {s.name}{s.status !== 'verified' ? ` (${s.status} — receives nothing)` : ''}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="Filter">
          <Select value={filterType} onChange={e => setFilterType(e.target.value as FilterType)} className="w-full">
            <option value="all">all — every event on this listener</option>
            <option value="routing_key_in">routing_key_in — specific asset ids</option>
            <option value="jsonpath_match">jsonpath_match — payload conditions</option>
          </Select>
        </Field>

        {filterType === 'routing_key_in' && (
          <Field label="Asset ids" hint="Enter to add. A batch matches if it contains any of these.">
            <TagInput values={keys} onChange={setKeys} placeholder="e.g. 102290129340398" />
          </Field>
        )}

        {filterType === 'jsonpath_match' && (
          <Field label="Conditions" hint="JSON array of {path, op, value}. Ops: eq, neq, in, exists, contains. All must match.">
            <textarea
              value={expr}
              onChange={e => setExpr(e.target.value)}
              rows={4}
              className="w-full rounded border border-ink-600 bg-ink-900 px-2 py-1 font-mono text-[11px] text-ink-100 focus:border-info-500 focus:outline-none"
            />
          </Field>
        )}

        <label className="flex items-start gap-2">
          <input type="checkbox" checked={isDefault} onChange={e => setIsDefault(e.target.checked)} className="mt-0.5" />
          <span className="text-xs text-ink-200">
            Default subscription
            <span className="block text-[11px] text-ink-400">
              Receives events that matched no other subscription, so nothing is silently dropped.
            </span>
          </span>
        </label>

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
            {busy ? 'Creating…' : 'Create subscription'}
          </Button>
        </div>
      </form>
    </Modal>
  )
}

/** Tag input for asset ids — they are pasted in batches and need to be
 * individually removable. */
function TagInput({ values, onChange, placeholder }: {
  values: string[]; onChange: (v: string[]) => void; placeholder?: string
}) {
  const [draft, setDraft] = useState('')

  function commit() {
    // Accept comma- or space-separated pastes as several tags.
    const parts = draft.split(/[\s,]+/).map(s => s.trim()).filter(Boolean)
    if (parts.length === 0) return
    onChange([...new Set([...values, ...parts])])
    setDraft('')
  }

  return (
    <div className="rounded border border-ink-600 bg-ink-900 p-1.5">
      {values.length > 0 && (
        <div className="mb-1.5 flex flex-wrap gap-1">
          {values.map(v => (
            <span key={v} className="inline-flex items-center gap-1 rounded bg-ink-800 px-1.5 py-0.5 font-mono text-[11px] text-ink-200">
              {v}
              <button
                type="button"
                onClick={() => onChange(values.filter(x => x !== v))}
                className="text-ink-400 hover:text-bad-400"
                aria-label={`Remove ${v}`}
              >✕</button>
            </span>
          ))}
        </div>
      )}
      <input
        value={draft}
        onChange={e => setDraft(e.target.value)}
        onKeyDown={e => {
          if (e.key === 'Enter' || e.key === ',') { e.preventDefault(); commit() }
          // Backspace on an empty field removes the last tag.
          if (e.key === 'Backspace' && !draft && values.length) onChange(values.slice(0, -1))
        }}
        onBlur={commit}
        placeholder={placeholder}
        className="w-full bg-transparent px-1 py-0.5 font-mono text-xs text-ink-100 placeholder:text-ink-400 focus:outline-none"
      />
    </div>
  )
}
