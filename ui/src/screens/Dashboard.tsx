import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../lib/api'
import type { Stats } from '../lib/types'
import { useStream } from '../lib/useStream'
import { Banner, Pill, Spinner, useInterval } from '../components/ui'

export default function Dashboard() {
  const [stats, setStats] = useState<Stats | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [flash, setFlash] = useState(0)

  const load = useCallback(async () => {
    try {
      setStats(await api.stats())
      setError(null)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not load stats')
    }
  }, [])

  useEffect(() => { void load() }, [load])
  // Stats are aggregates, so they are polled rather than streamed; the stream
  // only nudges the counter so the operator sees activity between polls.
  useInterval(load, 10_000)

  useStream(useCallback(() => setFlash(n => n + 1), []))

  if (error) return <div className="p-4"><Banner>{error}</Banner></div>
  if (!stats) return <div className="flex items-center gap-2 p-4 text-xs text-ink-400"><Spinner /> Loading…</div>

  const w = stats.windows['24h']
  const disabled = stats.services.filter(s => s.status === 'disabled')
  const laggy = stats.queue.planner_lag_seconds > 60

  return (
    <div className="space-y-3 p-4">
      {disabled.length > 0 && (
        <Banner>
          <strong>{disabled.length} service{disabled.length > 1 ? 's' : ''} disabled by the circuit breaker.</strong>{' '}
          {disabled.map(s => s.name).join(', ')} — no events are being delivered.{' '}
          <Link to="/services" className="underline">Re-enable in Services</Link> once healthy.
        </Banner>
      )}

      {laggy && (
        <Banner tone="warn">
          <strong>Planner is {Math.round(stats.queue.planner_lag_seconds)}s behind.</strong>{' '}
          {stats.queue.unplanned_events} event(s) received but not yet routed. Webhooks are
          being accepted and not forwarded.
        </Banner>
      )}

      <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
        <Stat label="Events / 24h" value={w.events} sub={`${stats.windows['1h'].events} in the last hour`} />
        <Stat
          label="Delivery success"
          value={`${(w.success_rate * 100).toFixed(1)}%`}
          sub={`${w.success} of ${w.success + w.failed + w.dead} settled`}
          tone={w.success_rate >= 0.99 ? 'ok' : w.success_rate >= 0.9 ? 'warn' : 'bad'}
        />
        <Stat label="Failed" value={w.failed} tone={w.failed > 0 ? 'warn' : undefined}
              sub="4xx — will not retry" />
        <Stat label="Dead" value={w.dead} tone={w.dead > 0 ? 'bad' : undefined}
              sub="attempts exhausted" />
      </div>

      <section className="rounded-lg border border-ink-800 bg-ink-900">
        <header className="flex items-center justify-between border-b border-ink-800 px-3 py-2">
          <h2 className="text-xs font-semibold text-ink-200">Events per minute</h2>
          <span className="font-mono text-[11px] text-ink-400">last 60 min · {flash} live</span>
        </header>
        <div className="p-3">
          <Sparkline data={stats.events_per_minute} />
        </div>
      </section>

      <section className="rounded-lg border border-ink-800 bg-ink-900">
        <header className="flex items-center justify-between border-b border-ink-800 px-3 py-2">
          <h2 className="text-xs font-semibold text-ink-200">Service health</h2>
          <span className="font-mono text-[11px] text-ink-400">
            queue {stats.queue.pending_deliveries} pending
          </span>
        </header>
        {stats.services.length === 0 ? (
          <p className="px-3 py-6 text-center text-xs text-ink-400">
            No services yet. <Link to="/services" className="underline">Add one</Link> to start fanning out.
          </p>
        ) : (
          <div className="grid gap-2 p-3 sm:grid-cols-2 lg:grid-cols-3">
            {stats.services.map(s => (
              <div key={s.id} className="rounded border border-ink-800 bg-ink-850 p-2.5">
                <div className="flex items-center justify-between gap-2">
                  <span className="truncate text-xs font-medium text-ink-100">{s.name}</span>
                  <Pill status={s.status} />
                </div>
                <p className="mt-0.5 truncate font-mono text-[11px] text-ink-400" title={s.id}>{s.id}</p>
                <div className="mt-2 flex items-center gap-3 font-mono text-[11px] text-ink-400">
                  <span className="text-ok-400">{s.success_24h} ok</span>
                  <span className={s.failed_24h > 0 ? 'text-bad-400' : ''}>{s.failed_24h} fail</span>
                  {s.avg_latency_ms != null && <span>{s.avg_latency_ms}ms avg</span>}
                </div>
                {s.consecutive_failures > 0 && (
                  <p className="mt-1.5 font-mono text-[11px] text-warn-400">
                    {s.consecutive_failures} consecutive failures
                  </p>
                )}
                {s.disabled_reason && (
                  <p className="mt-1.5 text-[11px] text-bad-400">{s.disabled_reason}</p>
                )}
              </div>
            ))}
          </div>
        )}
      </section>
    </div>
  )
}

function Stat({ label, value, sub, tone }: {
  label: string; value: string | number; sub?: string; tone?: 'ok' | 'warn' | 'bad'
}) {
  const tones = { ok: 'text-ok-400', warn: 'text-warn-400', bad: 'text-bad-400' }
  return (
    <div className="rounded-lg border border-ink-800 bg-ink-900 p-3">
      <p className="text-[11px] uppercase tracking-wide text-ink-400">{label}</p>
      <p className={`mt-1 font-mono text-2xl font-semibold ${tone ? tones[tone] : 'text-ink-100'}`}>{value}</p>
      {sub && <p className="mt-0.5 text-[11px] text-ink-400">{sub}</p>}
    </div>
  )
}

/** Inline SVG bar chart. No charting dependency for a 60-point series. */
function Sparkline({ data }: { data: { at: string; count: number }[] }) {
  if (data.length === 0) return <p className="text-xs text-ink-400">No data</p>

  const max = Math.max(...data.map(d => d.count), 1)
  const width = 100
  const height = 40
  const barWidth = width / data.length

  return (
    <div>
      <svg viewBox={`0 0 ${width} ${height}`} preserveAspectRatio="none" className="h-20 w-full">
        {data.map((d, i) => {
          const h = d.count === 0 ? 0.4 : (d.count / max) * height
          return (
            <rect
              key={d.at}
              x={i * barWidth}
              y={height - h}
              width={Math.max(barWidth - 0.3, 0.4)}
              height={h}
              className={d.count > 0 ? 'fill-info-400' : 'fill-ink-700'}
            >
              <title>{`${new Date(d.at).toLocaleTimeString()} — ${d.count} events`}</title>
            </rect>
          )
        })}
      </svg>
      <div className="mt-1 flex justify-between font-mono text-[11px] text-ink-400">
        <span>{new Date(data[0].at).toLocaleTimeString()}</span>
        <span>peak {max}/min</span>
        <span>{new Date(data[data.length - 1].at).toLocaleTimeString()}</span>
      </div>
    </div>
  )
}
