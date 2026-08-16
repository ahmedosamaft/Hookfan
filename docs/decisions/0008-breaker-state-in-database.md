# 8. Circuit breaker state lives in the database

- **Status:** Accepted
- **Date:** 2026-08-16

## Context

After 20 consecutive failures a service is disabled and stops receiving deliveries. That counter has to live somewhere.

The reflex is an in-memory counter per service — a map in the dispatcher, no database write on the failure path.

## Decision

`services.consecutive_failures`, incremented in the **same transaction** that records the failed delivery. `disabled_at` and `disabled_reason` are set when the threshold is reached.

## Why not in memory

Two failure modes, both silent:

**Multiple replicas.** With N API instances each holding its own counter, a service must fail 20×N times before any single instance trips. The breaker's threshold quietly becomes a function of how many replicas are deployed — and nobody notices until a service that should have been cut off is still being hammered.

**Deploys.** An in-memory counter resets on restart. Deploys are correlated with instability, so the counter is most likely to be wiped exactly when it is closest to firing.

Sharing the counter transactionally with the delivery outcome means it cannot drift from what actually happened.

## Consequences

**Good.** The threshold means the same thing regardless of replica count. State survives restarts. An operator can inspect and reset it with SQL.

**Bad.** One row update per failed delivery, and concurrent failures to the same service contend on that row. Acceptable because failure is the rare path — on the success path the counter is only reset when non-zero.

**Also.** Disabling stops *new* planning; it does not cancel deliveries already in flight, so `consecutive_failures` can climb past the threshold before the queue drains. Observed reaching 72 in testing against a threshold of 20. Not a defect, but surprising if unexplained.

Re-enabling is deliberately manual: `PATCH /api/services/{id}` with `reset_breaker: true` clears the counter and returns the service to `pending`, requiring a fresh link handshake before traffic resumes. Automatic recovery would send a service that is still broken straight back into rotation.
