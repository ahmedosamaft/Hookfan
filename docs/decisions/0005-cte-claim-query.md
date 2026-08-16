# 5. Claim deliveries with a CTE, not `WHERE id IN (SELECT … FOR UPDATE SKIP LOCKED)`

- **Status:** Accepted
- **Date:** 2026-08-16
- **Amends:** the claim query in the original specification

## Context

The original specification gave the queue claim as:

```sql
UPDATE deliveries SET status='in_flight', attempt_count = attempt_count + 1
WHERE id IN (
  SELECT id FROM deliveries
  WHERE status IN ('pending') AND next_attempt_at <= now()
  ORDER BY next_attempt_at
  FOR UPDATE SKIP LOCKED LIMIT $1
) RETURNING *;
```

This is a widely copied form, and it is subtly wrong. The subquery and the outer `UPDATE` take **separate** locks. The subquery skips rows locked at the moment it runs, but the outer `UPDATE` then re-locks those ids and **blocks** — it does not skip — if another worker claimed one in between.

It does not double-claim: under `READ COMMITTED` the outer `UPDATE` re-checks its predicate after the lock wait, and a row already flipped to `in_flight` fails `status = 'pending'` and is dropped. The damage is latency, not correctness. Workers serialise behind each other under exactly the contention `SKIP LOCKED` was chosen to avoid.

## Decision

Use a CTE so the lock and the update are one statement:

```sql
WITH claimed AS (
    SELECT id FROM deliveries
     WHERE status = 'pending' AND next_attempt_at <= now()
     ORDER BY next_attempt_at
     FOR UPDATE SKIP LOCKED
     LIMIT $2
)
UPDATE deliveries d
   SET status = 'in_flight', attempt_count = d.attempt_count + 1,
       claimed_at = now(), claimed_by = $1
  FROM claimed c
 WHERE d.id = c.id
RETURNING d.id, d.event_id, d.attempt_count, d.service_id;
```

`ORDER BY` stays inside the locking `SELECT`, which keeps claim order stable and avoids lock-order inversion between workers.

## Consequences

Workers skip past each other instead of queueing. Verified by `TestConcurrentClaimsNeverDoubleDeliver`: eight concurrent workers over forty deliveries, each delivered exactly once.

A companion requirement follows. A claimed row that is never completed — because its worker crashed — stays `in_flight` forever. A reaper returns rows to `pending` once `claimed_at` is older than 5 minutes. That threshold must comfortably exceed any service's request timeout plus its `Retry-After` sleep, or a slow-but-alive delivery would be reclaimed and sent twice.
