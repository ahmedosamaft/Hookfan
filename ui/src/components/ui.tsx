import { useEffect, useRef, useState, type ReactNode } from 'react'

/* Status pill — the colours the spec calls for: pending amber, verified green,
 * failed red, disabled grey. */
const PILL: Record<string, string> = {
  verified: 'bg-ok-500/15 text-ok-400 border-ok-500/30',
  success: 'bg-ok-500/15 text-ok-400 border-ok-500/30',
  pending: 'bg-warn-500/15 text-warn-400 border-warn-500/30',
  in_flight: 'bg-info-500/15 text-info-400 border-info-500/30',
  failed: 'bg-bad-500/15 text-bad-400 border-bad-500/30',
  dead: 'bg-bad-500/25 text-bad-400 border-bad-500/50',
  disabled: 'bg-ink-700/50 text-ink-400 border-ink-600',
}

export function Pill({ status, children }: { status: string; children?: ReactNode }) {
  return (
    <span className={`inline-flex items-center rounded border px-1.5 py-0.5 font-mono text-[11px] leading-none ${
      PILL[status] ?? PILL.disabled
    }`}>
      {children ?? status}
    </span>
  )
}

export function Button({
  variant = 'default', size = 'sm', className = '', ...props
}: React.ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: 'default' | 'primary' | 'danger' | 'ghost'
  size?: 'sm' | 'xs'
}) {
  const variants = {
    default: 'bg-ink-800 hover:bg-ink-700 border-ink-600 text-ink-100',
    primary: 'bg-info-500 hover:bg-info-400 border-info-500 text-white',
    danger: 'bg-bad-500/20 hover:bg-bad-500/30 border-bad-500/40 text-bad-400',
    ghost: 'bg-transparent hover:bg-ink-800 border-transparent text-ink-300',
  }
  const sizes = { sm: 'px-2.5 py-1 text-xs', xs: 'px-1.5 py-0.5 text-[11px]' }
  return (
    <button
      className={`inline-flex items-center gap-1 rounded border font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-40 ${variants[variant]} ${sizes[size]} ${className}`}
      {...props}
    />
  )
}

export function Input({ className = '', ...props }: React.InputHTMLAttributes<HTMLInputElement>) {
  return (
    <input
      className={`w-full rounded border border-ink-600 bg-ink-900 px-2 py-1 font-mono text-xs text-ink-100 placeholder:text-ink-400 focus:border-info-500 focus:outline-none ${className}`}
      {...props}
    />
  )
}

export function Select({ className = '', ...props }: React.SelectHTMLAttributes<HTMLSelectElement>) {
  return (
    <select
      className={`rounded border border-ink-600 bg-ink-900 px-2 py-1 text-xs text-ink-100 focus:border-info-500 focus:outline-none ${className}`}
      {...props}
    />
  )
}

export function Field({ label, hint, children }: { label: string; hint?: string; children: ReactNode }) {
  return (
    <label className="block">
      <span className="mb-1 block text-[11px] font-medium uppercase tracking-wide text-ink-400">{label}</span>
      {children}
      {hint && <span className="mt-1 block text-[11px] text-ink-400">{hint}</span>}
    </label>
  )
}

/** Copy-to-clipboard button with inline confirmation — used for tokens, URLs,
 * and IDs, all of which get pasted elsewhere. */
export function CopyButton({ value, label = 'Copy' }: { value: string; label?: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <Button
      size="xs"
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(value)
        } catch {
          // Clipboard API needs a secure context; fall back to a selection.
          const ta = document.createElement('textarea')
          ta.value = value
          document.body.appendChild(ta)
          ta.select()
          document.execCommand('copy')
          ta.remove()
        }
        setCopied(true)
        setTimeout(() => setCopied(false), 1500)
      }}
    >
      {copied ? '✓ Copied' : label}
    </Button>
  )
}

export function Modal({
  open, onClose, title, children, wide,
}: { open: boolean; onClose: () => void; title: string; children: ReactNode; wide?: boolean }) {
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && onClose()
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, onClose])

  if (!open) return null
  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-black/70 p-6"
      onClick={onClose}
    >
      <div
        className={`mt-8 w-full rounded-lg border border-ink-700 bg-ink-900 shadow-2xl ${wide ? 'max-w-3xl' : 'max-w-lg'}`}
        onClick={e => e.stopPropagation()}
      >
        <div className="flex items-center justify-between border-b border-ink-800 px-4 py-2.5">
          <h2 className="text-sm font-semibold text-ink-100">{title}</h2>
          <Button variant="ghost" size="xs" onClick={onClose} aria-label="Close">✕</Button>
        </div>
        <div className="p-4">{children}</div>
      </div>
    </div>
  )
}

export function Banner({ tone = 'bad', children }: { tone?: 'bad' | 'warn' | 'info'; children: ReactNode }) {
  const tones = {
    bad: 'border-bad-500/40 bg-bad-500/10 text-bad-400',
    warn: 'border-warn-500/40 bg-warn-500/10 text-warn-400',
    info: 'border-info-500/40 bg-info-500/10 text-info-400',
  }
  return <div className={`rounded border px-3 py-2 text-xs ${tones[tone]}`}>{children}</div>
}

export function Spinner() {
  return <span className="inline-block h-3 w-3 animate-spin rounded-full border-2 border-ink-600 border-t-info-400" />
}

export function EmptyState({ title, hint }: { title: string; hint?: string }) {
  return (
    <div className="px-4 py-12 text-center">
      <p className="text-sm text-ink-300">{title}</p>
      {hint && <p className="mt-1 text-xs text-ink-400">{hint}</p>}
    </div>
  )
}

/** Monospace value with optional copy affordance. IDs, tokens, and payloads
 * are always monospace so they can be compared character by character. */
export function Mono({ children, className = '' }: { children: ReactNode; className?: string }) {
  return <span className={`font-mono text-xs ${className}`}>{children}</span>
}

export function relativeTime(iso: string): string {
  const seconds = (Date.now() - new Date(iso).getTime()) / 1000
  if (seconds < 60) return `${Math.floor(seconds)}s ago`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`
  return `${Math.floor(seconds / 86400)}d ago`
}

export function useInterval(callback: () => void, ms: number | null) {
  const saved = useRef(callback)
  saved.current = callback
  useEffect(() => {
    if (ms === null) return
    const id = setInterval(() => saved.current(), ms)
    return () => clearInterval(id)
  }, [ms])
}
