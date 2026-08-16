# 1. Postgres only — no Redis, Kafka, or message broker

- **Status:** Accepted
- **Date:** 2026-08-16

## Context

hookfan needs a durable delivery queue: events arrive, fan out to N services, and retry on failure with backoff. The obvious reach is for a broker — Redis for the queue, or Kafka for a durable log.

The gateway is self-hosted. Every additional service is another thing an operator installs, monitors, secures, backs up, and debugs at 2am.

## Decision

Postgres is the only datastore. The delivery queue is a table, claimed with `FOR UPDATE SKIP LOCKED`.

## Consequences

**Good.** `docker compose up` starts two containers, not four. One backup covers both event history and queue state. A delivery and the event it belongs to are updated in the same transaction, so the queue can never disagree with the data it refers to — a property that is genuinely hard to get when the queue lives in a separate system.

**Bad.** Throughput is bounded by Postgres. `SKIP LOCKED` scales to thousands of deliveries per second on modest hardware, which is far beyond what a webhook fan-out gateway sees, but it is not Kafka. A deployment needing six-figure throughput would have to revisit this.

**Also.** The queue must be polled. `LISTEN`/`NOTIFY` reduces latency but is not a delivery guarantee — notifications sent while a listener is disconnected are lost — so a poll fallback is mandatory rather than a nicety. See [ADR 0006](0006-planner-stage.md).
