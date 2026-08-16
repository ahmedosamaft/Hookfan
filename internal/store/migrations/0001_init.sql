-- Initial hookfan schema.
--
-- Primary keys are bigserial: the delivery queue's hot index is
-- (status, next_attempt_at) and events are append-heavy, both of which favour
-- narrow, sequential keys over random UUIDs. Where an id crosses the trust
-- boundary (service ids appear in URLs and in the link-verify body sent to
-- third-party backends) a random public_id is used instead, so a sequence
-- position never leaks event or service volume.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE listeners (
    id                     bigserial PRIMARY KEY,
    name                   text        NOT NULL,
    slug                   text        NOT NULL UNIQUE,
    provider               text        NOT NULL DEFAULT 'meta'
                             CHECK (provider IN ('meta', 'generic')),
    verification_mode      text        NOT NULL DEFAULT 'hmac_sha256'
                             CHECK (verification_mode IN ('none', 'hmac_sha256')),
    signature_header       text        NOT NULL DEFAULT 'X-Hub-Signature-256',
    signature_prefix       text        NOT NULL DEFAULT 'sha256=',
    -- AES-GCM ciphertext (nonce prepended); never returned by the API.
    secret                 bytea,
    challenge_verify_token text,
    routing_key_path       text        NOT NULL DEFAULT 'entry[*].id',
    enabled                boolean     NOT NULL DEFAULT true,
    created_at             timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE services (
    id                   bigserial PRIMARY KEY,
    -- External identity: random 16 bytes base64url. Used in /api/services/{public_id}
    -- and as the service_id in the link-verify body.
    public_id            text        NOT NULL UNIQUE,
    name                 text        NOT NULL,
    url                  text        NOT NULL,
    method               text        NOT NULL DEFAULT 'POST'
                           CHECK (method IN ('POST', 'PUT', 'PATCH')),
    -- AES-GCM ciphertext; returned in plaintext exactly once, at create and rotate.
    link_token           bytea       NOT NULL,
    status               text        NOT NULL DEFAULT 'pending'
                           CHECK (status IN ('pending', 'verified', 'failed', 'disabled')),
    verified_at          timestamptz,
    last_verify_error    text,
    timeout_ms           integer     NOT NULL DEFAULT 10000 CHECK (timeout_ms > 0),
    max_attempts         integer     NOT NULL DEFAULT 6 CHECK (max_attempts > 0),
    rate_limit_rps       integer     NOT NULL DEFAULT 0 CHECK (rate_limit_rps >= 0),
    custom_headers       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    -- Circuit breaker state lives in the database, not in process memory: with
    -- multiple replicas an in-memory counter would need 20*N real failures to
    -- trip, and would reset on every deploy.
    consecutive_failures integer     NOT NULL DEFAULT 0,
    disabled_at          timestamptz,
    disabled_reason      text,
    enabled              boolean     NOT NULL DEFAULT true,
    created_at           timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE subscriptions (
    id           bigserial PRIMARY KEY,
    listener_id  bigint      NOT NULL REFERENCES listeners(id) ON DELETE CASCADE,
    service_id   bigint      NOT NULL REFERENCES services(id)  ON DELETE CASCADE,
    filter_type  text        NOT NULL DEFAULT 'all'
                   CHECK (filter_type IN ('all', 'routing_key_in', 'jsonpath_match')),
    routing_keys text[]      NOT NULL DEFAULT '{}',
    filter_expr  jsonb       NOT NULL DEFAULT '[]'::jsonb,
    -- Receives events that matched no other subscription on this listener.
    is_default   boolean     NOT NULL DEFAULT false,
    enabled      boolean     NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX subscriptions_listener_idx ON subscriptions (listener_id) WHERE enabled;
-- Hot direction: given an event's routing keys, find the subscriptions that
-- overlap them (the && operator).
CREATE INDEX subscriptions_routing_keys_idx ON subscriptions USING GIN (routing_keys);

CREATE TABLE events (
    id              bigserial PRIMARY KEY,
    listener_id     bigint      NOT NULL REFERENCES listeners(id) ON DELETE CASCADE,
    -- Every entry[].id in the batch. One event row per received request; the
    -- payload is never split, so raw_body stays byte-exact.
    routing_keys    text[]      NOT NULL DEFAULT '{}',
    -- bytea, never jsonb: jsonb reorders keys and normalises numbers, which
    -- would break both the byte-exactness promise and downstream signature
    -- re-verification.
    raw_body        bytea       NOT NULL,
    headers         jsonb       NOT NULL DEFAULT '{}'::jsonb,
    content_type    text,
    received_at     timestamptz NOT NULL DEFAULT now(),
    signature_valid boolean     NOT NULL DEFAULT false,
    dedupe_key      text,
    -- NULL until the planner has created this event's delivery set. A crashed
    -- planner leaves it NULL and the next pass retries.
    planned_at      timestamptz
);

-- Idempotent ingest: Meta redelivers the identical payload on timeout.
CREATE UNIQUE INDEX events_dedupe_idx ON events (listener_id, dedupe_key)
    WHERE dedupe_key IS NOT NULL;
-- The planner's claim query; partial so it stays tiny regardless of table size.
CREATE INDEX events_unplanned_idx ON events (id) WHERE planned_at IS NULL;
CREATE INDEX events_received_at_idx ON events (received_at DESC);
CREATE INDEX events_listener_received_idx ON events (listener_id, received_at DESC);
CREATE INDEX events_routing_keys_idx ON events USING GIN (routing_keys);

CREATE TABLE deliveries (
    id                 bigserial PRIMARY KEY,
    event_id           bigint      NOT NULL REFERENCES events(id)   ON DELETE CASCADE,
    service_id         bigint      NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    status             text        NOT NULL DEFAULT 'pending'
                         CHECK (status IN ('pending', 'in_flight', 'success', 'failed', 'dead')),
    attempt_count      integer     NOT NULL DEFAULT 0,
    next_attempt_at    timestamptz NOT NULL DEFAULT now(),
    claimed_at         timestamptz,
    claimed_by         text,
    last_status_code   integer,
    last_response_body text,
    last_error         text,
    latency_ms         integer,
    -- Which subscriptions caused this delivery. Deliveries are deduplicated to
    -- one per service, so without this the operator cannot answer "why did this
    -- service receive this event?".
    matched_subscription_ids bigint[] NOT NULL DEFAULT '{}',
    created_at         timestamptz NOT NULL DEFAULT now(),
    completed_at       timestamptz
);

-- Makes "exactly one delivery per service per event" a database invariant, so
-- a double-planned event cannot produce duplicate sends.
CREATE UNIQUE INDEX deliveries_event_service_idx ON deliveries (event_id, service_id);
-- The queue's hot path.
CREATE INDEX deliveries_claim_idx ON deliveries (status, next_attempt_at)
    WHERE status IN ('pending', 'in_flight');
CREATE INDEX deliveries_event_idx ON deliveries (event_id);
CREATE INDEX deliveries_service_created_idx ON deliveries (service_id, created_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Dependency order: deliveries and subscriptions reference the rest.
DROP TABLE IF EXISTS deliveries;
DROP TABLE IF EXISTS subscriptions;
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS services;
DROP TABLE IF EXISTS listeners;

-- +goose StatementEnd
