# 2. `bigserial` primary keys with a public identifier on services

- **Status:** Accepted
- **Date:** 2026-08-16
- **Supersedes:** the UUID primary keys in the original specification

## Context

The original specification used UUID primary keys throughout. Two tables are hot: `deliveries`, whose queue index `(status, next_attempt_at)` is scanned on every claim, and `events`, which is append-heavy.

Random UUIDs are poor B-tree keys for both. They are 16 bytes rather than 8, and random insertion order causes page splits and index fragmentation in exactly the tables that see the most writes.

Sequential integers fix that but leak information: an id in a URL is enumerable, and a monotonic id tells any observer how many events or services exist. For `services` this matters more than usual, because the id is sent to third-party backends in the link-verify body.

## Decision

`bigserial` primary keys and `bigint` foreign keys on all five tables.

`services` additionally carries `public_id` — 16 random bytes, base64url, prefixed `svc_` — used wherever the identifier crosses a trust boundary: the `/api/services/{public_id}` routes, the `service_id` field of the link-verify body, and the UI. Internal joins use the bigint.

Listeners are addressed by their existing `slug`. Events and deliveries stay bigint-addressed behind the admin bearer token.

## Alternatives rejected

**UUIDv7.** Time-ordered, so it solves the locality problem while staying non-enumerable. Rejected because it is still 16 bytes in the hottest index, and Postgres 16 has no built-in generator — it would need an extension or application-side generation.

**bigint everywhere with no public id.** Simplest, and defensible behind a bearer token, but it would have put a sequence position in the body sent to every third-party backend.

## Consequences

Narrower keys and sequential inserts in the two tables that need them. One extra unique-indexed column and one extra lookup on the service routes.

The asymmetry is a real cost: `services` is addressed by `public_id` in the API but by `id` internally, so handlers resolve one to the other. It is confined to the service handlers, which is why it was judged acceptable.
