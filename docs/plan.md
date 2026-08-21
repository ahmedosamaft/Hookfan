# hookfan — build plan

> **Status.** All seven phases are built and verified.
> This document is kept current as the design changes — see
> [spec-driven-development.md](spec-driven-development.md) for why, and
> [decisions/](decisions/) for the reasoning behind individual choices.
>
> Amendments made during implementation are marked **[amended]** inline.

## Context

Building a self-hosted gateway that receives webhooks from external providers (primarily Meta / WhatsApp Cloud API and Instagram Messaging), persists them, and fans them out to multiple registered backend services with retries and delivery tracking.

The problem it solves: Meta allows exactly one callback URL per app, but several internal services need the same events. Today that means either a bespoke forwarder per project or coupling every consumer to one service's ingest path. hookfan makes the callback URL a durable, observable fan-out point — an operator can see every event received, every delivery attempt, and replay any of them.

The repo is empty (one-line README, single initial commit). This is greenfield.

### Decisions taken before planning

Four choices were settled with the user and materially revise the original spec:

1. ~~**Go 1.23 installed on the host**~~ **[amended]** — pgx v5.10 requires Go ≥ 1.25, so the toolchain is **Go 1.25** in both the Dockerfile and locally. Go is installed at `~/.local/go`; the Makefile falls back to a `golang:1.25-alpine` container when no host toolchain is found.
2. **Full JSONPath library** (`github.com/ohler55/ojg`) for `routing_key_path` and `filter_expr` paths, not a hand-rolled dotted-path evaluator.
3. **No per-entry splitting.** One Event row per received HTTP request; `raw_body` is always the exact bytes received. `routing_key` becomes `routing_keys text[]` holding every `entry[].id` in the batch. Consequence: `X-Hookfan-Original-Signature` is always valid, because the forwarded body is never reconstructed.
4. **Delivery dedup by service.** Match subscriptions → dedup to a set of `service_id` → exactly one Delivery per service, regardless of how many subscriptions or routing keys matched. Without this, a service subscribed to two WABAs in one batch receives two identical POSTs.

Follow-on decisions: `dedupe_key = sha256(raw_body)` hex; `routing_key_in` matches by array overlap (`&&`); the events table shows first routing key + overflow count.

5. **`bigserial` primary keys, not UUIDs.** Narrower keys, better B-tree locality on the `(status, next_attempt_at)` queue index, and no random-insert page splits on the highest-volume tables. Because sequential ids in URLs are enumerable and leak volume, **services carry an additional random `public_id`** (16 bytes base64url, unique) used everywhere the id crosses the trust boundary: the `/api/services/{public_id}` routes, the `service_id` field of the link-verify body, and the UI. Internal joins and foreign keys use the bigint. Listeners are already addressed by `slug`; events and deliveries stay bigint-addressed behind the admin token.

---

## Architecture

### Ingest is dumb, planning is separate

The request goroutine does exactly: read raw body (capped) → verify HMAC → one INSERT → 200. No subscription matching, no delivery inserts, no outbound calls.

A **planner** stage owns fan-out. `events.planned_at timestamptz NULL`; ingest writes NULL, the planner claims unplanned events, matches subscriptions, inserts deliveries, and sets `planned_at` — all in **one transaction**. This gives crash-safety (a dead planner leaves `planned_at IS NULL`, the next pass retries), atomic delivery-set creation, and duplicate-safety for free since a deduped ingest never creates an event row at all.

Doing this synchronously on the request goroutine would put a join plus N inserts in the latency path; a DB trigger would be invisible and untestable.

**Planner lag is the primary health metric** — `max(now() - received_at) WHERE planned_at IS NULL`. If the planner wedges, ingest keeps returning 200 and nothing goes out. Exposed on `/api/readyz` and the dashboard.

### Queue claim

Use the CTE form, not `WHERE id IN (SELECT ... FOR UPDATE SKIP LOCKED)`. With the `IN` form the outer UPDATE re-locks the ids and *blocks* rather than skipping when another worker got there first (it does not double-claim — the re-checked predicate drops the row — but the lock wait stalls the worker).

```sql
WITH c AS (
  SELECT id FROM deliveries
   WHERE status = 'pending' AND next_attempt_at <= now()
   ORDER BY next_attempt_at
   FOR UPDATE SKIP LOCKED LIMIT $1
)
UPDATE deliveries d
   SET status='in_flight', attempt_count=d.attempt_count+1,
       claimed_at=now(), claimed_by=$2
  FROM c WHERE d.id = c.id
RETURNING d.*;
```

`ORDER BY` must be inside the locking SELECT. A **reaper** every 30s returns rows stuck `in_flight` past `claimed_at + 5min` to `pending`; that timeout must exceed any service's `timeout_ms` plus the `Retry-After` sleep ceiling, or a slow-but-alive delivery gets sent twice. The reaper does not use SKIP LOCKED.

### Safety net

Unique index on `deliveries (event_id, service_id)`, inserted with `ON CONFLICT DO NOTHING`. This makes "one delivery per service per event" a database invariant, so a double-planned event cannot produce duplicate sends.

`deliveries.matched_subscription_ids bigint[]` records which subscriptions caused the delivery — without it the operator cannot answer "why did this service get this event?" or "what stops flowing if I delete this subscription?" See [ADR 0004](decisions/0004-delivery-dedup-per-service.md).

### LISTEN/NOTIFY

A dedicated standalone `pgx.Conn` (not the pool, not `Hijack()`) runs `WaitForNotification`. On any error: close, backoff 200ms→5s, reconnect, **re-issue LISTEN, then force an unconditional poll** — notifications sent during the disconnect are gone forever. NOTIFY is a latency optimisation only; the 1s poll fallback is what makes it correct. Notifications and the ticker funnel into the same claim call with ~5ms coalescing so a burst of 500 webhooks doesn't cause 500 single-row claims.

NOTIFY fires *inside* the transaction (Postgres queues it to commit — firing after commit would lose it on crash).

### Circuit breaker

Counter lives on `services` (`consecutive_failures`, `disabled_at`), updated in the same tx that marks a delivery failed. In-memory would need 20×N real failures to trip across N replicas and would reset on every deploy. An in-memory read-through cache of `disabled`, invalidated over the same NOTIFY channel, keeps the hot path off the DB.

### Dedupe

`dedupe_key = sha256(raw_body)` hex, unique on `(listener_id, dedupe_key)`, `ON CONFLICT DO NOTHING`. Every WhatsApp message and status carries a unique `id`, so genuinely distinct events differ in bytes; byte-identical bodies are redeliveries. A counter increments on every conflict so that if real traffic is ever being eaten, it is visible rather than silent.

---

## Schema (migrations/0001_init.sql)

All primary keys are `bigserial`; all foreign keys `bigint`.

- **listeners** — id, name, slug (unique, immutable), provider, verification_mode, signature_header, signature_prefix, secret (AES-GCM bytea), challenge_verify_token, routing_key_path, enabled, created_at
- **services** — id, **public_id** (text, unique — random 16 bytes base64url, the external identity), name, url, method, link_token (encrypted), status, verified_at, timeout_ms (10000), max_attempts (6), rate_limit_rps, custom_headers jsonb, consecutive_failures, disabled_at, enabled
- **subscriptions** — id, listener_id, service_id, filter_type, routing_keys text[], filter_expr jsonb, is_default, enabled
- **events** — id, listener_id, routing_keys text[], raw_body **bytea** (never jsonb — jsonb reorders keys and normalises numbers, breaking the byte-exactness promise), headers jsonb, content_type, received_at, signature_valid, dedupe_key, **planned_at**
- **deliveries** — id, event_id, service_id, status, attempt_count, next_attempt_at, claimed_at, claimed_by, last_status_code, last_response_body (4KB), last_error, latency_ms, matched_subscription_ids bigint[], created_at, completed_at

Indexes: `deliveries (status, next_attempt_at)` (queue hot path), `deliveries (event_id, service_id)` unique, `events (listener_id, dedupe_key)` unique, `events (planned_at) WHERE planned_at IS NULL` partial, `events (received_at DESC)`, GIN on `subscriptions.routing_keys` (the hot direction: given event keys, find matching subs).

Migrations embedded via `embed.FS`, run on startup under a session-scoped `pg_advisory_lock` on a dedicated connection held across the whole run. Non-leaders **wait** on the lock and then verify schema version rather than skipping — otherwise a replica serves against a half-migrated schema.

**[amended]** Migrations are applied by [goose](https://github.com/pressly/goose) rather than a hand-rolled runner, using its `Provider` API (which avoids linking the SQLite driver that the package-level API pulls in). The advisory lock is still taken by hookfan rather than delegated to goose's own locker, so it covers the version check as well as the apply. Migration files live in `internal/store/migrations/` because `go:embed` cannot reach outside its own package directory. Operator commands: `hookfan migrate up|down|status`, wrapped as `make migrate-*`.

---

## File tree

```
cmd/hookfan/main.go              wiring, graceful shutdown
internal/config/                 env parse, fail-fast validation
internal/store/                  pgx queries, migrations embed, advisory lock
internal/crypto/                 AES-GCM, HMAC helpers, constant-time compare
internal/ingest/                 handler, sig verification, meta+generic adapters
internal/router/                 subscription matching, filter eval, jsonpath
internal/planner/                claim unplanned events -> deliveries (one tx)
internal/dispatch/               worker pool, claim, reaper, retry, breaker, ratelimit, SSRF guard
internal/api/                    admin handlers, CORS, SSE
internal/retention/              batched purge job
migrations/                      numbered .sql
ui/                              React app, nginx.conf, entrypoint.sh, Dockerfile
Dockerfile  docker-compose.yml  .env.example  Makefile  README.md
```

Dependencies (each justified): `pgx/v5` (Postgres, specified), `ojg` (JSONPath, chosen above), `golang.org/x/time/rate` (token bucket, specified), `pressly/goose` (migrations, **[amended]** — see above). Nothing else — with bigserial PKs there is no UUID dependency; `public_id` and `link_token` come from `crypto/rand`.

---

## Phase order

Each phase ends with a working demo and a stop for review.

**1 — Foundation.** ✅ *Done.* Go module, config with fail-fast validation (`SECRET_ENCRYPTION_KEY` must be 32 bytes base64, with a clear message), migrations + advisory lock, `/api/healthz` + `/api/readyz`, Dockerfile (distroless, <20MB), compose with db+api. *Demo: `docker compose up`, curl both health endpoints, show migrations applied.*

**2 — Ingest.** ✅ *Done.* Listener CRUD, Meta GET challenge (constant-time compare, raw `hub.challenge` as text/plain), POST ingest with `MaxBytesReader` at 1MB, HMAC over exact raw bytes before any decode, routing key extraction via JSONPath, dedupe insert. *Demo: curl with a hand-computed HMAC — valid, tampered, wrong secret, missing header; multi-entry batch showing both keys in `routing_keys`; same body twice → one row.*

**3 — Services, handshake, matching.** ✅ *Done.* Service CRUD addressed by `public_id`, link handshake with specific error classification (wrong challenge / timeout / DNS / TLS / connection refused / HTTP status — vague errors here waste hours), token rotation, subscriptions, filter evaluation for all three types plus default fallback, planner stage. *Demo: httptest receiver echoing the challenge; each failure mode shown distinctly; an event planned into deduped deliveries.*

**4 — Dispatch.** ✅ *Done.* Worker pool, CTE claim, reaper, full-jitter backoff (`rand(0, min(cap, base*2^n))` — the range starts at zero), `Retry-After` in both delta-seconds and HTTP-date forms, 4xx-terminal vs 5xx-retry, breaker, per-service rate limiter and shared `http.Client`, SSRF guard. *Demo: two dummy receivers, one failing — show retry schedule, terminal 400, breaker tripping at 20.*

**5 — Admin API + SSE.** ✅ *Done.* All endpoints, cursor pagination, replay, retry, stats, SSE. CORS from `ALLOWED_ORIGINS`, default-deny, never `*`. `secret` and `link_token` never in list responses; `link_token` returned only at create and rotate.

**6 — Frontend.** ✅ *Done.* React+Vite+TS+Tailwind, five screens, runtime `/config.js` via envsubst (not baked at build), nginx SPA fallback. Dense and monospace-heavy — the user is debugging at 2am. Receiver snippets in Go, Node, **Python/FastAPI** **[amended — added on request]**, and Java/Spring Boot.

**7 — Retention, README, `make demo`.** ✅ *Done.* Batched purge, README written last with the exact Meta app setup steps and the `API_BASE_URL` gotcha called out prominently (`http://api:8081` is wrong — the request originates in the browser).

---

## Tests

Table-driven, `httptest` for receivers, real Postgres via compose for store tests.

Signature verification (valid / tampered / wrong secret / missing header / malformed prefix) · GET challenge (correct / wrong / missing params) · multi-entry batch → one event with all routing keys · dedupe → one event, one delivery set · each filter type + default fallback · **fan-out dedup: a service matching two subscriptions in one batch gets exactly one delivery** · retry policy (4xx terminal, 5xx retried, backoff bounds, Retry-After both forms) · claim concurrency (no double-claim under parallel workers) · link handshake (success / wrong challenge / timeout / non-2xx) · end-to-end: post to a listener, assert both subscribers receive the byte-identical body and a valid gateway signature.

---

## Verification

Per phase: `make test` (unit) and `make up` + the curl sequence above. End-to-end at phase 7: `make demo` brings up db, api, ui, and two dummy receivers; post a signed Meta payload to a listener wired to both; the Events screen shows `2/2 delivered`, and each receiver logs a byte-identical body with a verifying `X-Hookfan-Signature`. Kill one receiver mid-run to watch retries, the breaker, and manual re-enable.

Image size targets checked with `docker images` (API <20MB, UI <60MB).
