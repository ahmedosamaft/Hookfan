# 6. A separate planner stage between ingest and delivery

- **Status:** Accepted
- **Date:** 2026-08-16

## Context

Ingest must be fast and must always answer `200`: Meta retries aggressively and disables callback URLs that misbehave. The target was a p99 under 10ms.

Fan-out needs subscription matching and N delivery inserts. Doing that on the request goroutine puts a join and several writes in the latency path, and makes ingest latency depend on how many subscribers a listener has.

But fan-out cannot simply be dropped either: an event that is accepted and then never planned is a silently lost webhook.

## Decision

A **planner** stage sits between them.

Ingest writes one row with `planned_at` NULL and returns 200. A background planner claims unplanned events, matches subscriptions, inserts deliveries, and sets `planned_at` — **all in one transaction**.

The transaction is what makes it safe. A planner that dies mid-plan leaves `planned_at` NULL, and the next pass retries the event. Delivery-set creation is atomic. A deduplicated ingest never creates an event row at all, so it never plans, which makes redelivery idempotent end to end.

## Alternatives rejected

**Plan synchronously during ingest.** Simplest, and correct, but puts the join and the inserts in the latency path.

**A database trigger.** Invisible to anyone reading the Go code, untestable in isolation, and impossible to rate-limit or instrument.

## Consequences

Ingest is a single INSERT. Measured p99 was 3.93ms over 200 requests, well inside the target.

**The cost is a new silent-failure mode**, and it is the important part of this decision. If the planner wedges, ingest keeps returning 200 while nothing is forwarded. The provider sees success. Subscribers see nothing. No error is logged, because nothing errored.

The mitigation is that **planner lag is the primary health metric**: `max(now() - received_at) WHERE planned_at IS NULL`, exposed on `/readyz`, which reports `degraded` past 60 seconds. Anyone operating hookfan should alert on it. A gateway that accepts webhooks and quietly forwards none is the worst failure this system can have, and lag is the only signal that distinguishes it from a quiet day.
