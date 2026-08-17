CREATE TABLE devices (
    id UUID PRIMARY KEY,
    display_name TEXT NOT NULL CHECK (char_length(display_name) BETWEEN 1 AND 100),
    created_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ
);

CREATE TABLE pairing_codes (
    lookup_id TEXT PRIMARY KEY,
    code_hash BYTEA NOT NULL CHECK (octet_length(code_hash) = 32),
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts INTEGER NOT NULL CHECK (max_attempts > 0),
    CHECK (expires_at > created_at)
);
CREATE INDEX pairing_codes_expiry_idx ON pairing_codes (expires_at) WHERE consumed_at IS NULL;

CREATE TABLE device_tokens (
    id UUID PRIMARY KEY,
    device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    token_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    scopes TEXT[] NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ
);
CREATE INDEX device_tokens_device_idx ON device_tokens (device_id);

CREATE TYPE outbox_status AS ENUM ('pending', 'processing', 'applied', 'dead');
CREATE TABLE outbox_messages (
    id UUID PRIMARY KEY,
    business_type TEXT NOT NULL,
    aggregate_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL UNIQUE,
    revision BIGINT NOT NULL CHECK (revision >= 0),
    generation BIGINT NOT NULL CHECK (generation >= 0),
    payload JSONB NOT NULL,
    audit_metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    status outbox_status NOT NULL DEFAULT 'pending',
    available_at TIMESTAMPTZ NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts INTEGER NOT NULL CHECK (max_attempts > 0),
    last_error_category TEXT,
    last_error_at TIMESTAMPTZ,
    lease_expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT outbox_aggregate_revision_generation UNIQUE
        (business_type, aggregate_id, revision, generation)
);
CREATE INDEX outbox_claim_idx ON outbox_messages (status, available_at, lease_expires_at, created_at)
    WHERE status IN ('pending', 'processing');
