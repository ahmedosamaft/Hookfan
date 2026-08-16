# 3. One event per request — no per-entry splitting

- **Status:** Accepted
- **Date:** 2026-08-16
- **Supersedes:** the multi-entry splitting behaviour in the original specification

## Context

A Meta webhook can carry several assets in one request:

```json
{"object":"whatsapp_business_account",
 "entry":[{"id":"WABA_ONE","changes":[…]},
          {"id":"WABA_TWO","changes":[…]}]}
```

The original specification called for splitting this into one Event row per entry, each wrapped in a reconstructed envelope so downstream services receive a valid Meta payload.

Reconstructing the envelope means re-marshalling JSON. Re-marshalled JSON differs from the received bytes — key order and whitespace change — and Meta's `X-Hub-Signature-256` is computed over the exact bytes. A split event therefore carries a signature that cannot validate against the body it accompanies, so `X-Hookfan-Original-Signature` would be dead weight on precisely the payloads where a subscriber most wants to check it.

## Decision

One Event row per received HTTP request. `raw_body` is always the exact bytes received, stored as `bytea`.

`routing_key` becomes `routing_keys text[]`, holding every `entry[].id` in the batch. Subscription matching uses array overlap (the `&&` operator), so a batch carrying two assets reaches subscribers of either.

Because a service can now match through several subscriptions at once, deliveries are deduplicated to **one per service** — see [ADR 0004](0004-delivery-dedup-per-service.md).

## Consequences

**Good.** The forwarded body is byte-identical to what the provider sent, so `X-Hookfan-Original-Signature` always validates downstream. Ingest gets cheaper: no parse-and-rebuild, just one insert.

**Bad.** A subscriber interested in only one asset receives the whole batch and must filter for itself. This is the honest trade — the alternative was handing them a payload whose signature does not verify.

**Also.** `raw_body` must be `bytea`, never `jsonb`. `jsonb` normalises key order and numeric representation, which would silently break byte-exactness. The column type is load-bearing.
