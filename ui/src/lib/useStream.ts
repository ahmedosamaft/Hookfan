import { useEffect, useRef, useState } from 'react'
import { apiBaseUrl, getToken } from './api'

export type StreamKind = 'event.received' | 'delivery.updated' | 'service.updated'

export interface StreamEvent {
  kind: StreamKind
  data: Record<string, unknown>
  at: number
}

/**
 * Subscribes to the SSE feed.
 *
 * EventSource cannot send an Authorization header, so the stream is read with
 * fetch and a manual parser. That also gives explicit control over reconnection
 * — EventSource retries silently and indefinitely, which hides an expired token
 * behind a reconnect loop.
 */
export function useStream(onEvent?: (e: StreamEvent) => void) {
  const [connected, setConnected] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const handlerRef = useRef(onEvent)
  handlerRef.current = onEvent

  useEffect(() => {
    const token = getToken()
    if (!token) return

    const controller = new AbortController()
    let retryDelay = 1000
    let stopped = false
    let retryTimer: ReturnType<typeof setTimeout> | undefined

    async function connect() {
      try {
        const response = await fetch(`${apiBaseUrl()}/api/stream`, {
          headers: { Authorization: `Bearer ${token}` },
          signal: controller.signal,
        })
        if (!response.ok || !response.body) {
          throw new Error(`stream returned HTTP ${response.status}`)
        }

        setConnected(true)
        setError(null)
        retryDelay = 1000

        const reader = response.body.getReader()
        const decoder = new TextDecoder()
        let buffer = ''

        for (;;) {
          const { done, value } = await reader.read()
          if (done) break
          buffer += decoder.decode(value, { stream: true })

          // SSE frames are separated by a blank line.
          let split: number
          while ((split = buffer.indexOf('\n\n')) !== -1) {
            const frame = buffer.slice(0, split)
            buffer = buffer.slice(split + 2)

            let kind = ''
            let payload = ''
            for (const line of frame.split('\n')) {
              if (line.startsWith('event: ')) kind = line.slice(7).trim()
              else if (line.startsWith('data: ')) payload += line.slice(6)
              // Lines starting with ':' are comments (keep-alives); ignored.
            }
            if (!kind || !payload) continue
            try {
              handlerRef.current?.({
                kind: kind as StreamKind,
                data: JSON.parse(payload),
                at: Date.now(),
              })
            } catch {
              // A malformed frame must not kill the stream.
            }
          }
        }
        throw new Error('stream closed by server')
      } catch (err) {
        if (stopped || controller.signal.aborted) return
        setConnected(false)
        setError(err instanceof Error ? err.message : 'stream error')
        // Capped exponential backoff so a restarting API is not hammered.
        retryTimer = setTimeout(connect, retryDelay)
        retryDelay = Math.min(retryDelay * 2, 30000)
      }
    }

    void connect()
    return () => {
      stopped = true
      clearTimeout(retryTimer)
      controller.abort()
    }
  }, [])

  return { connected, error }
}
