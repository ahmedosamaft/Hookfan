export type Provider = 'meta' | 'generic'
export type VerificationMode = 'none' | 'hmac_sha256'
export type ServiceStatus = 'pending' | 'verified' | 'failed' | 'disabled'
export type FilterType = 'all' | 'routing_key_in' | 'jsonpath_match'
export type DeliveryStatus = 'pending' | 'in_flight' | 'success' | 'failed' | 'dead'

export interface Listener {
  id: number
  name: string
  slug: string
  provider: Provider
  verification_mode: VerificationMode
  signature_header: string
  signature_prefix: string
  challenge_verify_token?: string
  routing_key_path: string
  enabled: boolean
  created_at: string
  has_secret: boolean
}

export interface Service {
  /** The public identifier (svc_…), not a sequence position. */
  id: string
  name: string
  url: string
  method: 'POST' | 'PUT' | 'PATCH'
  status: ServiceStatus
  verified_at?: string
  last_verify_error?: string
  timeout_ms: number
  max_attempts: number
  rate_limit_rps: number
  custom_headers: Record<string, string>
  consecutive_failures: number
  disabled_at?: string
  disabled_reason?: string
  enabled: boolean
  created_at: string
}

/** Returned only at creation and rotation — the token is never shown again. */
export interface ServiceWithToken {
  service: Service
  link_token: string
  warning: string
}

export interface VerifyResult {
  ok: boolean
  kind?: string
  message?: string
  status_code?: number
  latency_ms: number
  echoed?: string
}

export interface Subscription {
  id: number
  listener_id: number
  service_id: string
  filter_type: FilterType
  routing_keys: string[]
  filter_expr: FilterCondition[]
  is_default: boolean
  enabled: boolean
  created_at: string
}

export interface FilterCondition {
  path: string
  op: 'eq' | 'neq' | 'in' | 'exists' | 'contains'
  value?: unknown
}

export interface EventSummary {
  id: number
  listener_id: number
  listener_slug: string
  routing_keys: string[]
  content_type?: string
  received_at: string
  signature_valid: boolean
  planned: boolean
  body_bytes: number
  delivery_total: number
  delivery_success: number
  delivery_pending: number
  delivery_failed: number
  delivery_dead: number
}

export interface EventPage {
  events: EventSummary[]
  next_cursor?: string
  has_more: boolean
}

export interface Delivery {
  id: number
  event_id: number
  service_id: string
  status: DeliveryStatus
  attempt_count: number
  next_attempt_at: string
  claimed_at?: string
  last_status_code?: number
  last_response_body?: string
  last_error?: string
  latency_ms?: number
  matched_subscription_ids: number[]
  created_at: string
  completed_at?: string
}

export interface EventDetail {
  id: number
  listener_id: number
  listener_slug: string
  routing_keys: string[]
  content_type?: string
  received_at: string
  signature_valid: boolean
  planned_at?: string
  dedupe_key?: string
  body: string
  body_encoding: 'utf8' | 'base64'
  body_bytes: number
  deliveries: Delivery[]
}

export interface WindowStats {
  events: number
  events_invalid_signature: number
  deliveries: number
  success: number
  failed: number
  dead: number
  pending: number
  success_rate: number
}

export interface ServiceHealth {
  id: string
  name: string
  status: ServiceStatus
  enabled: boolean
  consecutive_failures: number
  disabled_at?: string
  disabled_reason?: string
  success_24h: number
  failed_24h: number
  avg_latency_ms?: number
}

export interface Stats {
  windows: Record<'1h' | '24h' | '7d', WindowStats>
  services: ServiceHealth[]
  queue: {
    pending_deliveries: number
    unplanned_events: number
    planner_lag_seconds: number
  }
  events_per_minute: { at: string; count: number }[]
}
