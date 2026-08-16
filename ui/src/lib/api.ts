import type {
  Delivery, EventDetail, EventPage, Listener, Service, ServiceWithToken,
  Stats, Subscription, VerifyResult,
} from './types'

declare global {
  interface Window {
    __HOOKFAN_CONFIG__?: { apiBaseUrl?: string }
  }
}

/**
 * The API base URL is read at boot from /config.js, which the container
 * entrypoint generates from an environment variable. Baking it in at build
 * time would pin the image to one environment.
 *
 * In development it falls back to a relative path, which Vite proxies.
 */
export function apiBaseUrl(): string {
  const configured = window.__HOOKFAN_CONFIG__?.apiBaseUrl?.trim()
  if (configured && configured !== '' && !configured.startsWith('${')) {
    return configured.replace(/\/$/, '')
  }
  return ''
}

const TOKEN_KEY = 'hookfan_admin_token'

/**
 * The admin token is held in memory and mirrored to sessionStorage so a page
 * refresh does not log the operator out. sessionStorage rather than
 * localStorage: the token should not outlive the browser session.
 */
let inMemoryToken: string | null = null

export function getToken(): string | null {
  if (inMemoryToken) return inMemoryToken
  inMemoryToken = sessionStorage.getItem(TOKEN_KEY)
  return inMemoryToken
}

export function setToken(token: string) {
  inMemoryToken = token
  sessionStorage.setItem(TOKEN_KEY, token)
}

export function clearToken() {
  inMemoryToken = null
  sessionStorage.removeItem(TOKEN_KEY)
}

export class ApiError extends Error {
  status: number
  /** Field-level messages, when the server reported several at once. */
  errors?: string[]
  /** The parsed response body, retained so callers can read a structured
   *  failure payload (e.g. the link-handshake result on a 422). */
  body?: unknown

  constructor(status: number, message: string, errors?: string[], body?: unknown) {
    super(message)
    this.status = status
    this.errors = errors
    this.body = body
  }
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const token = getToken()
  const headers = new Headers(init.headers)
  if (token) headers.set('Authorization', `Bearer ${token}`)
  if (init.body) headers.set('Content-Type', 'application/json')

  let response: Response
  try {
    response = await fetch(`${apiBaseUrl()}${path}`, { ...init, headers })
  } catch {
    // A network-level failure here is usually a wrong API_BASE_URL or CORS,
    // so say so rather than surfacing "Failed to fetch".
    throw new ApiError(0,
      `Cannot reach the API at ${apiBaseUrl() || 'the current origin'}. ` +
      `Check API_BASE_URL and that ALLOWED_ORIGINS includes ${window.location.origin}.`)
  }

  if (response.status === 401) {
    clearToken()
    throw new ApiError(401, 'Unauthorized — check the admin token.')
  }
  if (response.status === 204) return undefined as T

  const text = await response.text()
  let body: unknown
  try {
    body = text ? JSON.parse(text) : {}
  } catch {
    throw new ApiError(response.status, text || response.statusText)
  }

  if (!response.ok) {
    const e = body as { error?: string; errors?: string[] }
    throw new ApiError(response.status, e.error ?? response.statusText, e.errors, body)
  }
  return body as T
}

/**
 * Like request(), but treats the listed status codes as successful so their
 * response body is returned instead of being turned into an ApiError. Used
 * where a non-2xx response carries information the caller needs.
 */
async function requestRaw<T>(path: string, init: RequestInit, okStatuses: number[]): Promise<T> {
  try {
    return await request<T>(path, init)
  } catch (err) {
    if (err instanceof ApiError && okStatuses.includes(err.status) && err.body !== undefined) {
      return err.body as T
    }
    throw err
  }
}

export const api = {
  // Listeners
  listListeners: () =>
    request<{ listeners: Listener[] }>('/api/listeners').then(r => r.listeners),
  getListener: (id: number) => request<Listener>(`/api/listeners/${id}`),
  createListener: (body: Partial<Listener> & { secret?: string }) =>
    request<Listener>('/api/listeners', { method: 'POST', body: JSON.stringify(body) }),
  updateListener: (id: number, body: Record<string, unknown>) =>
    request<Listener>(`/api/listeners/${id}`, { method: 'PATCH', body: JSON.stringify(body) }),
  deleteListener: (id: number) =>
    request<void>(`/api/listeners/${id}`, { method: 'DELETE' }),

  // Services
  listServices: () =>
    request<{ services: Service[] }>('/api/services').then(r => r.services),
  createService: (body: Record<string, unknown>) =>
    request<ServiceWithToken>('/api/services', { method: 'POST', body: JSON.stringify(body) }),
  updateService: (id: string, body: Record<string, unknown>) =>
    request<Service>(`/api/services/${id}`, { method: 'PATCH', body: JSON.stringify(body) }),
  deleteService: (id: string) =>
    request<void>(`/api/services/${id}`, { method: 'DELETE' }),
  /**
   * Runs the link handshake.
   *
   * A failed handshake is a 422 whose body still carries the full result —
   * the specific kind (dns_error, wrong_challenge, …) and message. Those are
   * the whole point of the endpoint, so this reads the body on failure rather
   * than collapsing it to the generic HTTP status text.
   */
  verifyService: (id: string) =>
    requestRaw<{ service: Service; result: VerifyResult }>(
      `/api/services/${id}/verify`, { method: 'POST' }, [422],
    ),
  rotateToken: (id: string) =>
    request<ServiceWithToken>(`/api/services/${id}/rotate-token`, { method: 'POST' }),

  // Subscriptions
  listSubscriptions: (listenerId?: number) =>
    request<{ subscriptions: Subscription[] }>(
      `/api/subscriptions${listenerId ? `?listener_id=${listenerId}` : ''}`,
    ).then(r => r.subscriptions),
  createSubscription: (body: Record<string, unknown>) =>
    request<Subscription>('/api/subscriptions', { method: 'POST', body: JSON.stringify(body) }),
  updateSubscription: (id: number, body: Record<string, unknown>) =>
    request<Subscription>(`/api/subscriptions/${id}`, { method: 'PATCH', body: JSON.stringify(body) }),
  deleteSubscription: (id: number) =>
    request<void>(`/api/subscriptions/${id}`, { method: 'DELETE' }),

  // Events
  listEvents: (params: Record<string, string>) => {
    const q = new URLSearchParams(
      Object.entries(params).filter(([, v]) => v !== '' && v != null),
    )
    return request<EventPage>(`/api/events?${q}`)
  },
  getEvent: (id: number) => request<EventDetail>(`/api/events/${id}`),
  replayEvent: (id: number) =>
    request<{ event_id: number }>(`/api/events/${id}/replay`, { method: 'POST' }),
  retryDelivery: (id: number) =>
    request<{ delivery_id: number }>(`/api/deliveries/${id}/retry`, { method: 'POST' }),

  stats: () => request<Stats>('/api/stats'),
}

export type { Delivery }
