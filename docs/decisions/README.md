# Architecture Decision Records

One file per decision that was genuinely contested, recording the alternatives and why they lost.

These are **append-only**. A decision that is later reversed gets a new ADR marked as superseding the old one; the original is not rewritten. The point is to preserve the reasoning that was persuasive at the time — including the reasoning that turned out to be wrong.

Decisions that were obvious do not get an ADR. See [../spec-driven-development.md](../spec-driven-development.md) for why the bar is set there.

| # | Decision | Status |
|---|---|---|
| [0001](0001-postgres-only-no-broker.md) | Postgres only — no Redis, Kafka, or message broker | Accepted |
| [0002](0002-bigserial-primary-keys.md) | `bigserial` primary keys with a public identifier on services | Accepted |
| [0003](0003-no-event-splitting.md) | One event per request — no per-entry splitting | Accepted |
| [0004](0004-delivery-dedup-per-service.md) | Exactly one delivery per service per event | Accepted |
| [0005](0005-cte-claim-query.md) | Claim deliveries with a CTE, not `WHERE id IN (…)` | Accepted |
| [0006](0006-planner-stage.md) | A separate planner stage between ingest and delivery | Accepted |
| [0007](0007-dedupe-by-body-hash.md) | Deduplicate ingest by `sha256(raw_body)` | Accepted |
| [0008](0008-breaker-state-in-database.md) | Circuit breaker state lives in the database | Accepted |

## Writing a new one

Copy the shape of an existing file: **Context** (the forces at play), **Decision** (what was chosen), **Alternatives rejected** where they existed, and **Consequences** — including the bad ones. An ADR that lists only benefits is not a record, it is an advertisement.

Number sequentially. Keep it short; if it runs past a page, the decision probably wants splitting.
