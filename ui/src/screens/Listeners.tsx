import { useCallback, useEffect, useState } from 'react'
import { api, ApiError, apiBaseUrl } from '../lib/api'
import type { Listener } from '../lib/types'
import {
  Banner, Button, CopyButton, EmptyState, Field, Input, Modal, Mono, Pill, Select, Spinner,
} from '../components/ui'

export default function Listeners() {
  const [listeners, setListeners] = useState<Listener[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [creating, setCreating] = useState(false)
  const [created, setCreated] = useState<Listener | null>(null)

  const load = useCallback(async () => {
    try {
      setListeners(await api.listListeners())
      setError(null)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not load listeners')
    }
  }, [])
  useEffect(() => { void load() }, [load])

  async function remove(l: Listener) {
    if (!confirm(`Delete listener "${l.name}"?\n\nIts events, subscriptions, and delivery history are deleted too. This cannot be undone.`)) return
    try {
      await api.deleteListener(l.id)
      await load()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not delete listener')
    }
  }

  async function toggle(l: Listener) {
    try {
      await api.updateListener(l.id, { enabled: !l.enabled })
      await load()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not update listener')
    }
  }

  return (
    <div className="space-y-3 p-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-sm font-semibold text-ink-100">Listeners</h1>
          <p className="text-xs text-ink-400">Inbound endpoints a provider posts to.</p>
        </div>
        <Button variant="primary" onClick={() => setCreating(true)}>New listener</Button>
      </div>

      {error && <Banner>{error}</Banner>}

      <div className="overflow-hidden rounded-lg border border-ink-800">
        <table className="w-full text-left">
          <thead className="bg-ink-850 text-[11px] uppercase tracking-wide text-ink-400">
            <tr>
              <th className="w-14 px-3 py-1.5 font-medium">ID</th>
              <th className="px-3 py-1.5 font-medium">Name</th>
              <th className="px-3 py-1.5 font-medium">Callback URL</th>
              <th className="px-3 py-1.5 font-medium">Provider</th>
              <th className="px-3 py-1.5 font-medium">Verification</th>
              <th className="px-3 py-1.5 font-medium">Routing path</th>
              <th className="px-3 py-1.5 font-medium"></th>
            </tr>
          </thead>
          <tbody className="divide-y divide-ink-800 bg-ink-900">
            {listeners === null && (
              <tr><td colSpan={7} className="px-3 py-6 text-center text-xs text-ink-400"><Spinner /></td></tr>
            )}
            {listeners?.length === 0 && (
              <tr><td colSpan={7}>
                <EmptyState title="No listeners yet"
                  hint="Create one to get a callback URL to paste into the Meta app dashboard." />
              </td></tr>
            )}
            {listeners?.map(l => (
              <tr key={l.id} className="hover:bg-ink-850">
                <td className="px-3 py-1.5">
                  {/* The listener id is what /api/subscriptions?listener_id=
                      takes, so it is worth showing rather than hiding. */}
                  <Mono className="text-ink-400">#{l.id}</Mono>
                </td>
                <td className="px-3 py-1.5">
                  <div className="flex items-center gap-2">
                    <span className="text-xs text-ink-100">{l.name}</span>
                    {!l.enabled && <Pill status="disabled">disabled</Pill>}
                  </div>
                </td>
                <td className="px-3 py-1.5">
                  <div className="flex items-center gap-1.5">
                    <Mono className="text-ink-300">/hooks/{l.slug}</Mono>
                    <CopyButton value={callbackUrl(l.slug)} label="⧉" />
                  </div>
                </td>
                <td className="px-3 py-1.5"><Mono className="text-ink-300">{l.provider}</Mono></td>
                <td className="px-3 py-1.5">
                  <Mono className={l.verification_mode === 'none' ? 'text-warn-400' : 'text-ink-300'}>
                    {l.verification_mode}
                  </Mono>
                </td>
                <td className="px-3 py-1.5"><Mono className="text-ink-400">{l.routing_key_path}</Mono></td>
                <td className="px-3 py-1.5 text-right">
                  <div className="flex justify-end gap-1">
                    <Button size="xs" onClick={() => setCreated(l)}>Setup</Button>
                    <Button size="xs" onClick={() => toggle(l)}>{l.enabled ? 'Disable' : 'Enable'}</Button>
                    <Button size="xs" variant="danger" onClick={() => remove(l)}>Delete</Button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <CreateListener
        open={creating}
        onClose={() => setCreating(false)}
        onCreated={async l => { setCreating(false); setCreated(l); await load() }}
      />
      <SetupPanel listener={created} onClose={() => setCreated(null)} />
    </div>
  )
}

function callbackUrl(slug: string): string {
  const base = apiBaseUrl() || window.location.origin
  return `${base}/hooks/${slug}`
}

function CreateListener({ open, onClose, onCreated }: {
  open: boolean; onClose: () => void; onCreated: (l: Listener) => void
}) {
  const [form, setForm] = useState({
    name: '', slug: '', provider: 'meta',
    secret: '', challenge_verify_token: '', routing_key_path: 'entry[*].id',
  })
  const [errors, setErrors] = useState<string[]>([])
  const [busy, setBusy] = useState(false)

  const set = (k: keyof typeof form) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setForm(f => ({ ...f, [k]: e.target.value }))

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setErrors([])
    try {
      const created = await api.createListener({
        ...form,
        provider: form.provider as 'meta' | 'generic',
        verification_mode: form.secret ? 'hmac_sha256' : 'none',
      })
      setForm({ name: '', slug: '', provider: 'meta', secret: '', challenge_verify_token: '', routing_key_path: 'entry[*].id' })
      onCreated(created)
    } catch (err) {
      if (err instanceof ApiError) setErrors(err.errors ?? [err.message])
      else setErrors([String(err)])
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal open={open} onClose={onClose} title="New listener">
      <form onSubmit={submit} className="space-y-3">
        <Field label="Name">
          <Input value={form.name} onChange={set('name')} placeholder="WhatsApp Production" autoFocus />
        </Field>
        <Field label="Slug" hint="Appears in the callback URL and cannot be changed later.">
          <Input value={form.slug} onChange={set('slug')} placeholder="whatsapp-prod" />
        </Field>
        <Field label="Provider">
          <Select value={form.provider} onChange={set('provider')} className="w-full">
            <option value="meta">meta — WhatsApp / Instagram</option>
            <option value="generic">generic</option>
          </Select>
        </Field>
        <Field label="App secret" hint="Used to verify X-Hub-Signature-256. Encrypted at rest; never shown again.">
          <Input type="password" value={form.secret} onChange={set('secret')} placeholder="from the Meta app dashboard" />
        </Field>
        {form.provider === 'meta' && (
          <Field label="Verify token" hint="Any string you choose. Meta sends it back during the GET handshake.">
            <Input value={form.challenge_verify_token} onChange={set('challenge_verify_token')} placeholder="a token you invent" />
          </Field>
        )}
        <Field label="Routing key path" hint="JSONPath to the asset id. Meta batches put it at entry[*].id.">
          <Input value={form.routing_key_path} onChange={set('routing_key_path')} />
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
            {busy ? 'Creating…' : 'Create listener'}
          </Button>
        </div>
      </form>
    </Modal>
  )
}

/** Shows the callback URL and verify token together, which is exactly the pair
 * the Meta app dashboard asks for. */
function SetupPanel({ listener, onClose }: { listener: Listener | null; onClose: () => void }) {
  if (!listener) return null
  const url = callbackUrl(listener.slug)

  return (
    <Modal open onClose={onClose} title={`Set up ${listener.name}`} wide>
      <div className="space-y-4">
        {listener.provider === 'meta' && (
          <div className="rounded border border-info-500/30 bg-info-500/5 p-3">
            <p className="mb-2 text-xs font-medium text-info-400">
              Paste this into the Meta app dashboard
            </p>
            <p className="mb-3 text-[11px] text-ink-400">
              App Dashboard → your app → WhatsApp → Configuration → Edit webhook.
              Meta immediately sends a GET to verify the token before saving.
            </p>

            <div className="space-y-2">
              <LabeledValue label="Callback URL" value={url} />
              <LabeledValue
                label="Verify token"
                value={listener.challenge_verify_token ?? '(not set — edit the listener)'}
              />
            </div>
          </div>
        )}

        <div>
          <p className="mb-1.5 text-[11px] uppercase tracking-wide text-ink-400">Test the handshake</p>
          <pre className="overflow-x-auto rounded border border-ink-700 bg-ink-950 p-2.5 font-mono text-[11px] text-ink-200">
{`curl "${url}?hub.mode=subscribe\\
&hub.verify_token=${listener.challenge_verify_token ?? 'YOUR_TOKEN'}\\
&hub.challenge=test123"

# Expected output: test123`}
          </pre>
        </div>

        {!listener.has_secret && (
          <Banner tone="warn">
            No app secret is set, so signatures are not verified. Anyone who knows
            the URL can post events to this listener.
          </Banner>
        )}
      </div>
    </Modal>
  )
}

function LabeledValue({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <p className="mb-0.5 text-[11px] uppercase tracking-wide text-ink-400">{label}</p>
      <div className="flex items-center gap-1.5">
        <code className="flex-1 overflow-x-auto rounded border border-ink-700 bg-ink-950 px-2 py-1 font-mono text-xs text-ink-100">
          {value}
        </code>
        <CopyButton value={value} />
      </div>
    </div>
  )
}
