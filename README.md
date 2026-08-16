# hookfan

A lightweight, self-hosted webhook fan-out gateway.

hookfan receives webhooks from external providers — primarily the Meta / WhatsApp Cloud API and Instagram Messaging — persists them, and forwards them to any number of registered backend services with retries, delivery tracking, and replay.

It exists because Meta allows exactly one callback URL per app. When several internal services need the same events, the alternatives are a bespoke forwarder per project or coupling every consumer to one service's ingest path. hookfan makes the callback URL a durable, observable fan-out point instead.

```
                  ┌──────────────┐        ┌─────────────────┐
  Meta ──POST──▶  │   hookfan    │ ─────▶ │  orders-api     │
  webhook         │              │ ─────▶ │  analytics      │
                  │  ingest      │ ─────▶ │  notifications  │
                  │  → plan      │        └─────────────────┘
                  │  → deliver   │
                  └──────┬───────┘
                         │  every event, every attempt, replayable
                    ┌────▼────┐
                    │ Postgres │
                    └──────────┘
```

## What it does

- **Verifies** the provider's HMAC signature over the exact received bytes.
- **Persists** every event, byte-for-byte, before anything else happens.
- **Fans out** to the services whose subscriptions match, exactly once each.
- **Retries** with exponential backoff and full jitter, honouring `Retry-After`.
- **Stops** hammering a service that is down, via a circuit breaker.
- **Shows you** every event, every delivery attempt, and every failure reason, with replay and per-delivery retry.

## Quick start

Requires Docker with Compose.

```bash
git clone https://github.com/ahmedosamaft/Hookfan.git
cd Hookfan
cp .env.example .env
```

Generate the two required secrets and put them in `.env`:

```bash
echo "ADMIN_TOKEN=$(openssl rand -base64 32)"
echo "SECRET_ENCRYPTION_KEY=$(openssl rand -base64 32)"
```

Then:

```bash
make up      # db + api + ui
make demo    # two receivers, one webhook, fan-out to both
```

`make demo` is the fastest way to see it work. It creates a listener, starts two receiver containers, completes the link handshake for each, posts a signed webhook, and shows it arriving at both — plus a tampered copy being rejected.

The UI is at **http://localhost:8080** (sign in with your `ADMIN_TOKEN`), the API at **http://localhost:8081**.

---

## ⚠️ The `API_BASE_URL` gotcha

**Setting `API_BASE_URL=http://api:8080` will not work.** This is the mistake everyone makes first.

The UI is a browser application. When it calls the API, **the request comes from your browser, not from the UI container.** `api` is a Docker Compose service name that only resolves inside the compose network — your browser has never heard of it.

`API_BASE_URL` must be the address **you would type into your address bar**:

| Where you run it | Correct value |
|---|---|
| Local machine | `http://localhost:8081` |
| Server, direct | `http://192.0.2.10:8081` |
| Behind a domain | `https://hookfan-api.example.com` |

Two things must agree, or the browser will block the request:

1. `API_BASE_URL` — where the UI sends requests
2. `ALLOWED_ORIGINS` on the API — must include the origin the UI is *served from*

The container warns loudly at startup if you get this wrong. If the UI shows "Cannot reach the API", check these two values first.

---

## Pointing a Meta app at hookfan

**1. Create a listener.** In the UI, **Listeners → New listener**:

| Field | Value |
|---|---|
| Slug | `whatsapp-prod` — appears in the callback URL, immutable |
| Provider | `meta` |
| App secret | From **Meta App Dashboard → App settings → Basic → App secret** |
| Verify token | Any string you invent. You will paste the same value into Meta. |

**2. Make the callback URL reachable from the internet.** Meta must be able to POST to it, over **HTTPS with a valid certificate**. Locally, use a tunnel:

```bash
cloudflared tunnel --url http://localhost:8081
# or: ngrok http 8081
```

**3. Register it with Meta.** In the App Dashboard → **WhatsApp → Configuration → Edit** webhook:

- **Callback URL**: `https://your-domain/hooks/whatsapp-prod`
- **Verify token**: the value from step 1

Meta immediately sends a `GET` with `hub.challenge`. hookfan echoes it back and Meta saves the configuration. If it fails, the listener's Setup panel in the UI has a `curl` command that reproduces the exact handshake.

**4. Subscribe to webhook fields** — `messages` at minimum — in the same Meta screen.

**5. Confirm.** Send a WhatsApp message to your business number; it should appear in the UI's Events screen within a second.

## Adding a backend service

**1. Services → New service.** Give it a name and the URL that should receive events.

**2. Copy the link token.** It is shown **once**, in a modal, alongside a working receiver implementation in Go, Node, Python, or Java. Copy both.

**3. Implement the receiver.** Your endpoint must:

- Compare `X-Hookfan-Token` against the link token, in constant time
- Verify `X-Hookfan-Signature` (`sha256=<hex>`) as an HMAC of the **raw body**
- When `X-Hookfan-Event: link.verify`, respond `{"challenge": "<the challenge>"}`
- Otherwise return `2xx` quickly and do the work asynchronously

The snippet in the UI does all of this — it is the same code `make demo` runs.

**4. Click Verify.** On failure the UI shows the exact reason — `dns_error`, `connection_refused`, `wrong_challenge`, `tls_error`, `timeout`, or the HTTP status — not a generic failure.

**5. Subscribe it.** A verified service still receives nothing until a subscription joins it to a listener.

## Headers on every forwarded request

| Header | Meaning |
|---|---|
| `X-Hookfan-Token` | Your link token — authenticates the gateway |
| `X-Hookfan-Signature` | `sha256=<hmac of the body using your link token>` |
| `X-Hookfan-Event` | `link.verify` or `event.forward` |
| `X-Hookfan-Event-Id` | Stable id, useful for your own idempotency |
| `X-Hookfan-Delivery-Id` | This attempt-set's id |
| `X-Hookfan-Listener` | The listener slug the event arrived on |
| `X-Hookfan-Attempt` | 1-based attempt number |
| `X-Hookfan-Original-Signature` | The provider's own signature, still valid — the body is never rewritten |

## Retry behaviour

| Response | What happens |
|---|---|
| `2xx` | Success |
| `408`, `429` | Retried; `429` honours `Retry-After` (seconds or HTTP-date) |
| Other `4xx` | **Terminal** — a 400 will still be a 400 on attempt six |
| `5xx`, timeout, connection error | Retried with backoff to `max_attempts`, then `dead` |

Backoff is exponential with **full jitter**: `random(0, min(1h, 2s × 2^attempt))`. The random range starting at zero is what stops a fleet of failed deliveries retrying in lockstep and stampeding a service as it recovers.

After **20 consecutive failures** a service is disabled and stops receiving events. Re-enabling is deliberately manual — Services → Re-enable, then Verify.

## Configuration

Every variable is documented in [.env.example](.env.example). The ones that matter:

| Variable | Default | Notes |
|---|---|---|
| `ADMIN_TOKEN` | — | **Required.** Guards the whole admin API. |
| `SECRET_ENCRYPTION_KEY` | — | **Required.** 32 bytes base64. Encrypts secrets at rest; changing it makes existing secrets unreadable. |
| `API_BASE_URL` | `http://localhost:8081` | Browser-reachable API address. See the gotcha above. |
| `ALLOWED_ORIGINS` | `http://localhost:8080` | Comma-separated. Defaults to deny; `*` is rejected. |
| `WORKER_CONCURRENCY` | `16` | Delivery worker goroutines. |
| `EVENT_RETENTION_DAYS` | `30` | Events and deliveries older than this are purged hourly, in batches. |
| `ALLOW_PRIVATE_TARGETS` | `true` in compose | Permits forwarding to RFC1918 addresses. Needed on a compose network; set `false` when targets should be public. |

## Operating it

**Watch planner lag.** It is the single most important metric:

```bash
curl localhost:8081/readyz
```

```json
{"checks":{"database":"ok","planner":"ok","planner_lag_seconds":0},"status":"ok"}
```

The planner turns received events into delivery sets. If it wedges, **ingest keeps returning 200 while nothing is forwarded** — the provider sees success and your services see silence. `/readyz` reports `degraded` past 60 seconds of lag. Alert on it.

`/healthz` is a liveness probe and does not touch the database, so a database blip never restarts the container.

**Scaling.** The API is stateless apart from Postgres. Run several replicas behind a load balancer: migrations are guarded by an advisory lock, the delivery queue uses `FOR UPDATE SKIP LOCKED`, and the circuit breaker counter lives in the database precisely so it behaves the same across replicas.

## Development

```bash
make toolchain         # which Go is in use (host, or a container)
make test              # unit tests
make test-integration  # throwaway Postgres, full suite
make fmt vet
make ui-dev            # Vite dev server on :5173
make migrate-status
```

Go is optional — the Makefile falls back to a `golang:1.25-alpine` container.

## Documentation

| | |
|---|---|
| [docs/plan.md](docs/plan.md) | Architecture, schema, phase plan |
| [docs/decisions/](docs/decisions/) | Architecture Decision Records — why things are the way they are |
| [docs/spec-driven-development.md](docs/spec-driven-development.md) | How this project is specified and built |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Setup, tests, and areas that need care |

## Status

Phases 1–7 of the [build plan](docs/plan.md) are complete: ingest, the link handshake, subscription matching, the planner, the delivery worker, the admin API with SSE, the web UI, and the retention job.

It has not yet been run in production. The Events table is not virtualised and paginates at 200 rows, which is fine at moderate volume but would want virtualising for very large histories.

## License

[Apache License 2.0](LICENSE).
