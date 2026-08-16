# Contributing to hookfan

Thanks for your interest. This document covers how to get the project running, what is expected of a change, and how decisions get made.

hookfan is developed **spec-anchored**: for anything beyond a small fix, the design is agreed in writing before the code is written, and the written design is kept current afterwards. [docs/spec-driven-development.md](docs/spec-driven-development.md) explains what that means in practice and — importantly — how much process each size of change actually warrants. A typo fix needs none of it.

## Getting set up

You need Docker with Compose. Go is optional: the Makefile uses a local toolchain when it finds one and falls back to a `golang:1.25-alpine` container otherwise.

```bash
git clone https://github.com/ahmedosamaft/Hookfan.git
cd Hookfan
cp .env.example .env

# Fill in the two required secrets:
#   ADMIN_TOKEN=$(openssl rand -base64 32)
#   SECRET_ENCRYPTION_KEY=$(openssl rand -base64 32)

make up
curl localhost:8081/healthz
```

`make toolchain` reports which Go it is using. A local install is considerably faster — the full test suite runs in about a second natively versus roughly twenty through the container.

## Everyday commands

```bash
make test              # unit tests; database-backed tests skip themselves
make test-integration  # spins up a throwaway Postgres, runs everything, tears it down
make fmt               # gofmt -w
make vet               # go vet
make build             # compile check
make up / down / logs  # the compose stack
make psql              # psql shell against the running database
make migrate-status    # which migrations are applied
```

Migrations run automatically at startup under a Postgres advisory lock, so multiple replicas cannot race. `make migrate-up`, `migrate-down`, and `migrate-create NAME=add_something` are there for working on them by hand.

## What a change should include

**Tests are how behaviour is specified.** A behavioural change without a test is incomplete — not as a matter of coverage targets, but because the test is the durable statement of what must hold. `TestConcurrentClaimsNeverDoubleDeliver` *is* the specification of the claim query; prose asserting the same thing would only be a claim.

Run `make test-integration` before submitting. Database-backed tests skip without `TEST_DATABASE_URL`, so `make test` alone can pass while integration tests fail.

**Match the surrounding code.** Comments here explain *why*, not *what* — the reasoning that is not recoverable from reading the statement. Study a file before adding to it.

**Update the docs that your change invalidates.** If you alter the schema, the queue mechanics, or a protocol, `docs/plan.md` needs to match. A stale plan is worse than none, because people trust it.

**Add an ADR when you reject a real alternative.** New file in `docs/decisions/`, next number, following the existing shape: Context, Decision, Consequences, and the alternatives that lost. Only for decisions that were genuinely contested — an ADR for an obvious choice is noise. Never rewrite an existing ADR to reflect a later change; add a new one marked as superseding it, so the history of the reasoning survives.

## Areas that need care

Some parts of this codebase have non-obvious constraints. Changing them without knowing why they are that way will break something subtle.

**The ingest path** ([internal/ingest/handler.go](internal/ingest/handler.go)) must stay fast and must return `200` on any successfully verified webhook — including when no subscription matches. Meta retries aggressively and disables callback URLs that return errors. No fan-out, no outbound calls, no work beyond one insert on the request goroutine.

**`raw_body` is byte-exact and must remain so.** It is `bytea`, never `jsonb` — `jsonb` normalises key order and numeric representation. The provider's HMAC covers the exact bytes received, so any re-marshalling invalidates `X-Hookfan-Original-Signature` downstream. Never re-encode a payload.

**Signature comparison is constant-time.** `hmac.Equal` and `subtle.ConstantTimeCompare`, never `==`. This applies to the Meta signature and to the challenge verify token.

**The queue claim query** ([internal/store/deliveries.go](internal/store/deliveries.go)) uses a CTE deliberately. The more common `WHERE id IN (SELECT … FOR UPDATE SKIP LOCKED)` form makes the outer `UPDATE` block on concurrent workers instead of skipping — see [ADR 0005](docs/decisions/0005-cte-claim-query.md).

**Secrets are encrypted at rest** and must never appear in an API response or a log line. `link_token` is returned exactly twice in its life: at creation and at rotation. When logging a rejected webhook, log the body's hash — never the body, which may contain message content.

**Planner lag is the health signal.** If you touch the planner, keep `/readyz` honest. A wedged planner means webhooks are accepted and silently never forwarded, and lag is the only thing that distinguishes that from a quiet day ([ADR 0006](docs/decisions/0006-planner-stage.md)).

## Submitting

1. Branch from `master`.
2. Make the change, with tests.
3. `make fmt && make vet && make test-integration`.
4. Open a PR describing what changed and why. For anything substantial, link or include the design you agreed beforehand.

For a large or architectural change, **open an issue first**. Agreeing the approach before implementation is the cheapest step in this process; reversing a design decision after it is built is the most expensive.

Commit messages: a concise imperative subject line, and a body explaining *why* when it is not obvious.

## Reporting bugs

Include what you expected, what happened, and enough to reproduce it — the listener configuration, a payload (with secrets redacted), and the relevant log lines. `LOG_LEVEL=debug` produces more detail.

**Do not open a public issue for a security vulnerability.** Contact the maintainer directly so a fix can be prepared before disclosure.

## License

Contributions are licensed under [Apache License 2.0](LICENSE), matching the project. By submitting a change you confirm you have the right to license it that way.
