import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../lib/api'
import type { Delivery, EventDetail, EventSummary, Listener } from '../lib/types'
import { useStream } from '../lib/useStream'
import {
  Banner, Button, CopyButton, EmptyState, Input, Mono, Pill, Select, Spinner, relativeTime,
} from '../components/ui'

export default function Events() {
  const [events, setEvents] = useState<EventSummary[]>([])
  const [listeners, setListeners] = useState<Listener[]>([])
  const [cursor, setCursor] = useState<string | undefined>()
  const [hasMore, setHasMore] = useState(false)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [expanded, setExpanded] = useState<number | null>(null)
  const [live, setLive] = useState(true)

  const [search, setSearch] = useState('')
  const [listenerId, setListenerId] = useState('')
  const [sigFilter, setSigFilter] = useState('')

  const load = useCallback(async (opts: { append?: boolean; cursor?: string } = {}) => {
    setLoading(true)
    try {
      const page = await api.listEvents({
        limit: '50',
        cursor: opts.cursor ?? '',
        listener_id: listenerId,
        routing_key: search.trim(),
        signature_valid: sigFilter,
      })
      setEvents(prev => (opts.append ? [...prev, ...page.events] : page.events))
      setCursor(page.next_cursor)
      setHasMore(page.has_more)
      setError(null)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not load events')
    } finally {
      setLoading(false)
    }
  }, [listenerId, search, sigFilter])

  useEffect(() => { void api.listListeners().then(setListeners).catch(() => {}) }, [])

  // Debounced so typing in the search box does not fire a request per keystroke.
  useEffect(() => {
    const t = setTimeout(() => void load(), 250)
    return () => clearTimeout(t)
  }, [load])

  // New events are prepended live, but only on the first page and only when no
  // filter is active — otherwise a streamed event could violate the filter or
  // appear out of order relative to a cursor.
  const onStream = useCallback((e: { kind: string; data: Record<string, unknown> }) => {
    if (!live || e.kind !== 'event.received') return
    if (search || listenerId || sigFilter) return
    const d = e.data as unknown as EventSummary
    setEvents(prev => {
      if (prev.some(x => x.id === d.id)) return prev
      return [{ ...d, delivery_total: 0, delivery_success: 0, delivery_pending: 0,
                delivery_failed: 0, delivery_dead: 0, planned: false }, ...prev].slice(0, 200)
    })
  }, [live, search, listenerId, sigFilter])
  useStream(onStream)

  return (
    <div className="space-y-3 p-4">
      <div className="flex flex-wrap items-end gap-2">
        <div className="flex-1">
          <h1 className="text-sm font-semibold text-ink-100">Events</h1>
          <p className="text-xs text-ink-400">
            Press <kbd className="rounded border border-ink-600 bg-ink-800 px-1 font-mono text-[10px]">/</kbd> to search.
          </p>
        </div>

        <Input
          id="event-search"
          value={search}
          onChange={e => setSearch(e.target.value)}
          placeholder="filter by routing key…"
          className="w-56"
        />
        <Select value={listenerId} onChange={e => setListenerId(e.target.value)}>
          <option value="">all listeners</option>
          {listeners.map(l => <option key={l.id} value={l.id}>{l.slug}</option>)}
        </Select>
        <Select value={sigFilter} onChange={e => setSigFilter(e.target.value)}>
          <option value="">any signature</option>
          <option value="true">valid only</option>
          <option value="false">invalid only</option>
        </Select>
        <Button size="sm" onClick={() => setLive(l => !l)} title="Prepend new events as they arrive">
          {live ? '⏸ pause live' : '▶ resume live'}
        </Button>
      </div>

      {error && <Banner>{error}</Banner>}

      <div className="overflow-hidden rounded-lg border border-ink-800">
        <table className="w-full text-left">
          <thead className="bg-ink-850 text-[11px] uppercase tracking-wide text-ink-400">
            <tr>
              <th className="w-6 px-2 py-1.5"></th>
              <th className="px-3 py-1.5 font-medium">Received</th>
              <th className="px-3 py-1.5 font-medium">Listener</th>
              <th className="px-3 py-1.5 font-medium">Routing keys</th>
              <th className="px-3 py-1.5 font-medium">Sig</th>
              <th className="px-3 py-1.5 font-medium">Delivered</th>
              <th className="px-3 py-1.5 font-medium">Size</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-ink-800 bg-ink-900">
            {loading && events.length === 0 && (
              <tr><td colSpan={7} className="px-3 py-6 text-center"><Spinner /></td></tr>
            )}
            {!loading && events.length === 0 && (
              <tr><td colSpan={7}>
                <EmptyState title="No events"
                  hint="Post a webhook to /hooks/{slug} and it will appear here." />
              </td></tr>
            )}
            {events.map(e => (
              <EventRow
                key={e.id}
                event={e}
                expanded={expanded === e.id}
                onToggle={() => setExpanded(expanded === e.id ? null : e.id)}
              />
            ))}
          </tbody>
        </table>
      </div>

      {hasMore && (
        <div className="flex justify-center">
          <Button onClick={() => void load({ append: true, cursor })} disabled={loading}>
            {loading ? <Spinner /> : 'Load more'}
          </Button>
        </div>
      )}
    </div>
  )
}

function EventRow({ event, expanded, onToggle }: {
  event: EventSummary; expanded: boolean; onToggle: () => void
}) {
  const [detail, setDetail] = useState<EventDetail | null>(null)
  const [loading, setLoading] = useState(false)
  const loadedFor = useRef<number | null>(null)

  // Detail is fetched only when a row is opened: the list would otherwise
  // carry every payload on screen.
  useEffect(() => {
    if (!expanded || loadedFor.current === event.id) return
    setLoading(true)
    api.getEvent(event.id)
      .then(d => { setDetail(d); loadedFor.current = event.id })
      .catch(() => {})
      .finally(() => setLoading(false))
  }, [expanded, event.id])

  const summary = event.delivery_total === 0
    ? '—'
    : `${event.delivery_success}/${event.delivery_total}`
  const tone = event.delivery_total === 0 ? 'text-ink-400'
    : event.delivery_dead > 0 || event.delivery_failed > 0 ? 'text-bad-400'
    : event.delivery_success === event.delivery_total ? 'text-ok-400'
    : 'text-warn-400'

  return (
    <>
      <tr className="cursor-pointer hover:bg-ink-850" onClick={onToggle}>
        <td className="px-2 py-1 text-center text-ink-400">{expanded ? '▾' : '▸'}</td>
        <td className="px-3 py-1">
          <Mono className="text-ink-200">{new Date(event.received_at).toLocaleTimeString()}</Mono>
          <span className="ml-1.5 text-[11px] text-ink-400">{relativeTime(event.received_at)}</span>
        </td>
        <td className="px-3 py-1"><Mono className="text-ink-300">{event.listener_slug}</Mono></td>
        <td className="px-3 py-1">
          {/* First key plus an overflow count keeps the row height uniform. */}
          {event.routing_keys.length === 0 ? (
            <Mono className="text-ink-400">—</Mono>
          ) : (
            <span className="flex items-center gap-1">
              <Mono className="text-ink-200">{event.routing_keys[0]}</Mono>
              {event.routing_keys.length > 1 && (
                <span className="rounded bg-ink-800 px-1 font-mono text-[10px] text-ink-400">
                  +{event.routing_keys.length - 1}
                </span>
              )}
            </span>
          )}
        </td>
        <td className="px-3 py-1">
          {event.signature_valid
            ? <Mono className="text-ok-400">ok</Mono>
            : <Mono className="text-bad-400">BAD</Mono>}
        </td>
        <td className="px-3 py-1"><Mono className={tone}>{summary}</Mono></td>
        <td className="px-3 py-1"><Mono className="text-ink-400">{event.body_bytes}B</Mono></td>
      </tr>

      {expanded && (
        <tr className="bg-ink-950">
          <td colSpan={7} className="px-4 py-3">
            {loading && <Spinner />}
            {detail && <EventDetailPanel detail={detail} />}
          </td>
        </tr>
      )}
    </>
  )
}

function EventDetailPanel({ detail }: { detail: EventDetail }) {
  const [deliveries, setDeliveries] = useState<Delivery[]>(detail.deliveries)
  const [busy, setBusy] = useState(false)
  const [note, setNote] = useState<string | null>(null)

  async function replay() {
    setBusy(true)
    try {
      await api.replayEvent(detail.id)
      setNote('Replaying — deliveries are rebuilt from the subscriptions currently in force.')
      setTimeout(async () => {
        const fresh = await api.getEvent(detail.id)
        setDeliveries(fresh.deliveries)
      }, 1200)
    } catch (err) {
      setNote(err instanceof Error ? err.message : 'Replay failed')
    } finally {
      setBusy(false)
    }
  }

  async function retry(d: Delivery) {
    setBusy(true)
    try {
      await api.retryDelivery(d.id)
      setNote(`Delivery ${d.id} requeued.`)
      setTimeout(async () => {
        const fresh = await api.getEvent(detail.id)
        setDeliveries(fresh.deliveries)
      }, 1200)
    } catch (err) {
      setNote(err instanceof Error ? err.message : 'Retry failed')
    } finally {
      setBusy(false)
    }
  }

  const pretty = (() => {
    if (detail.body_encoding === 'base64') return '(binary payload — base64)\n' + detail.body
    try {
      return JSON.stringify(JSON.parse(detail.body), null, 2)
    } catch {
      return detail.body
    }
  })()

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <Mono className="text-ink-400">event #{detail.id}</Mono>
        {detail.dedupe_key && (
          <Mono className="text-ink-400" >dedupe {detail.dedupe_key.slice(0, 12)}…</Mono>
        )}
        {detail.routing_keys.length > 1 && (
          <Mono className="text-ink-300">keys: {detail.routing_keys.join(', ')}</Mono>
        )}
        <div className="ml-auto flex gap-1">
          <CopyButton value={detail.body} label="Copy body" />
          <Button size="xs" onClick={replay} disabled={busy}>Replay</Button>
        </div>
      </div>

      {note && <Banner tone="info">{note}</Banner>}

      <div className="grid gap-3 lg:grid-cols-2">
        <div>
          <p className="mb-1 text-[11px] uppercase tracking-wide text-ink-400">Payload</p>
          <pre className="max-h-72 overflow-auto rounded border border-ink-800 bg-ink-900 p-2 font-mono text-[11px] leading-relaxed text-ink-200">
            {pretty}
          </pre>
        </div>

        <div>
          <p className="mb-1 text-[11px] uppercase tracking-wide text-ink-400">
            Deliveries ({deliveries.length})
          </p>
          {deliveries.length === 0 ? (
            <p className="rounded border border-ink-800 bg-ink-900 px-2 py-3 text-center text-[11px] text-ink-400">
              No deliveries — no subscription matched, or the event is not yet planned.
            </p>
          ) : (
            <div className="space-y-1.5">
              {deliveries.map(d => (
                <div key={d.id} className="rounded border border-ink-800 bg-ink-900 p-2">
                  <div className="flex items-center gap-2">
                    <Pill status={d.status} />
                    <Mono className="text-ink-300">{d.service_id.slice(0, 16)}…</Mono>
                    <Mono className="ml-auto text-ink-400">
                      att {d.attempt_count}
                      {d.last_status_code != null && ` · ${d.last_status_code}`}
                      {d.latency_ms != null && ` · ${d.latency_ms}ms`}
                    </Mono>
                    {(d.status === 'failed' || d.status === 'dead') && (
                      <Button size="xs" onClick={() => retry(d)} disabled={busy}>Retry</Button>
                    )}
                  </div>
                  {d.last_error && (
                    <p className="mt-1 font-mono text-[11px] text-bad-400">{d.last_error}</p>
                  )}
                  {d.last_response_body && (
                    <pre className="mt-1 max-h-24 overflow-auto rounded bg-ink-950 p-1.5 font-mono text-[10px] text-ink-300">
                      {d.last_response_body}
                    </pre>
                  )}
                  {d.matched_subscription_ids.length > 0 && (
                    <p className="mt-1 font-mono text-[10px] text-ink-400">
                      matched by subscription {d.matched_subscription_ids.join(', ')}
                    </p>
                  )}
                </div>
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
