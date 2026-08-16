# 4. Exactly one delivery per service per event

- **Status:** Accepted
- **Date:** 2026-08-16

## Context

Since an event now carries every routing key in a batch ([ADR 0003](0003-no-event-splitting.md)), one event can match a single service through more than one subscription. A service subscribed separately to `WABA_ONE` and `WABA_TWO` matches twice when a batch contains both.

Naively creating one delivery per matching subscription would send that service two identical POSTs for one webhook. Duplicate delivery of a message-received event is not a cosmetic problem — it is a duplicate message in whatever the subscriber builds.

## Decision

Matching collapses to a **set of service ids**, and one delivery is created per service regardless of how many subscriptions matched.

Two mechanisms enforce it:

1. The planner deduplicates by service id before inserting.
2. A unique index on `deliveries (event_id, service_id)`, inserted with `ON CONFLICT DO NOTHING`, makes it a database invariant. Even a double-planned event — a crash between the delivery insert and the `planned_at` commit, or an operator re-running a backfill — cannot produce duplicate sends.

`deliveries.matched_subscription_ids bigint[]` records every subscription that caused the delivery.

## Why the provenance column exists

Deduplicating discards *why* a service received an event. Without recording it, an operator cannot answer "which subscription caused this delivery?" or "what stops flowing if I delete this subscription?"

It costs one array column and is populated during matching, when the information is already in hand. Reconstructing it afterwards would mean re-evaluating every filter against the stored payload.

## Consequences

A service receives exactly one POST per event, whatever the subscription topology. The database enforces it independently of planner correctness — the belt-and-braces that makes the invariant hold even when the code above it is wrong.

Verified by `TestPlanDeduplicatesDeliveriesPerService` and demonstrated end to end: a two-asset batch produced one delivery for the doubly-matched service, with `matched_by {1,2}`.
