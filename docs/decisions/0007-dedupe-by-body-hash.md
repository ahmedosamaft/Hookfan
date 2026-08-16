# 7. Deduplicate ingest by `sha256(raw_body)`

- **Status:** Accepted
- **Date:** 2026-08-16

## Context

Meta redelivers a webhook when it does not see a timely `200`. Without deduplication, a slow response produces duplicate events and therefore duplicate deliveries to every subscriber.

The original specification derived the dedupe key from the message or status id inside `changes[].value`. That no longer works now that one event covers a whole batch ([ADR 0003](0003-no-event-splitting.md)) — a batch may contain many such ids, or none.

## Decision

`dedupe_key = sha256(raw_body)`, hex-encoded. A unique index on `(listener_id, dedupe_key)` with `ON CONFLICT DO NOTHING` makes ingest idempotent.

A duplicate still returns `200`: from the provider's perspective the event was accepted the first time, and any other status risks the callback URL being disabled.

## The objection, and why it was declined

A review raised that byte-identical bodies might represent genuinely distinct events, so hashing the body would silently drop real traffic.

That is wrong for this payload shape. Every WhatsApp message and status carries a unique `id` — `wamid.…` — inside `changes[].value`. Two genuinely distinct events therefore differ in bytes, and their hashes differ. Bodies that are byte-identical are redeliveries of the same event, which is exactly what should be collapsed.

The objection is not *baseless*, though: it holds for any provider whose payloads carry no unique identifier and no timestamp finer than the redelivery window. hookfan does not have such a provider today.

## Consequences

Ingest is idempotent with no assumptions about payload structure beyond "distinct events differ in bytes" — which also means it works for `generic` listeners, not only Meta.

**Every suppression is logged** at INFO with the body hash and a counter. This is the concession to the objection: if the assumption is ever wrong for some provider, the evidence is in the logs rather than invisible. A silent drop would be indistinguishable from no traffic.

Alternative considered: sorting and joining every message id in the batch. Rejected as Meta-shape-specific, and it would need to fall back to a body hash whenever no ids were present — the same mechanism, with a fragile special case bolted on.
