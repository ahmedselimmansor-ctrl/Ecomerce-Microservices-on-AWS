-- identity-service schema.
--
-- Two tables carry the security posture: `credentials` (how a password is
-- stored) and `refresh_tokens` (how a session is rotated and revoked).

CREATE TABLE users (
    id             TEXT PRIMARY KEY,                      -- usr_<ULID>
    email          TEXT        NOT NULL,
    -- Citext would be neater, but the extension is not available on every
    -- managed Postgres tier. A functional unique index does the same job and
    -- is portable.
    full_name      TEXT        NOT NULL,
    locale         TEXT        NOT NULL DEFAULT 'en-GB',
    phone          TEXT,
    email_verified BOOLEAN     NOT NULL DEFAULT FALSE,
    phone_verified BOOLEAN     NOT NULL DEFAULT FALSE,
    enabled        BOOLEAN     NOT NULL DEFAULT TRUE,
    mfa_enabled    BOOLEAN     NOT NULL DEFAULT FALSE,
    mfa_secret     TEXT,                                  -- encrypted at the application layer
    accepted_terms_version TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Case-insensitive uniqueness. Two accounts differing only by capitalisation
-- is an account-takeover vector at the password-reset step.
CREATE UNIQUE INDEX users_email_uniq ON users (lower(email));

CREATE TABLE credentials (
    user_id       TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    -- Argon2id, encoded PHC string. The parameters live inside the hash, so a
    -- future cost increase re-hashes on next login without a migration.
    password_hash TEXT        NOT NULL,
    algorithm     TEXT        NOT NULL DEFAULT 'argon2id',
    -- Forces a re-hash on next successful login when the cost parameters move.
    needs_rehash  BOOLEAN     NOT NULL DEFAULT FALSE,
    changed_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Blocks reuse of recent passwords without storing them in a reversible
    -- form. Array of Argon2id hashes, newest first, capped at 5.
    previous_hashes TEXT[]    NOT NULL DEFAULT '{}'
);

CREATE TABLE roles (
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role    TEXT NOT NULL CHECK (role IN ('CUSTOMER','MERCHANT','SUPPORT','OPS','ADMIN')),
    granted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    granted_by TEXT,
    PRIMARY KEY (user_id, role)
);

-- ---------------------------------------------------------------------------
-- Refresh tokens.
--
-- The `parent_id` chain plus `session_id` is what makes reuse detection
-- possible: one login is one session, each rotation links to its predecessor,
-- and revoking a family kills every token derived from that login.

CREATE TABLE refresh_tokens (
    id          TEXT PRIMARY KEY,
    user_id     TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    session_id  TEXT        NOT NULL,
    parent_id   TEXT REFERENCES refresh_tokens(id),
    -- SHA-256 of 256 bits of CSPRNG output. A database dump must not hand
    -- over live sessions. Not Argon2id: there is no dictionary to attack and
    -- no reason to pay a slow KDF on the refresh path.
    token_hash  TEXT        NOT NULL UNIQUE,
    state       TEXT        NOT NULL DEFAULT 'ACTIVE'
                CHECK (state IN ('ACTIVE','USED','REVOKED')),
    revoked_reason TEXT,
    device_fingerprint TEXT,
    amr         TEXT[]      NOT NULL DEFAULT '{}',
    ip_address  INET,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    used_at     TIMESTAMPTZ,
    expires_at  TIMESTAMPTZ NOT NULL,

    CONSTRAINT used_has_timestamp CHECK (state <> 'USED' OR used_at IS NOT NULL),
    CONSTRAINT revoked_has_reason CHECK (state <> 'REVOKED' OR revoked_reason IS NOT NULL)
);

-- The hot path: look up by hash on every refresh.
CREATE INDEX refresh_tokens_session_idx ON refresh_tokens (session_id);
CREATE INDEX refresh_tokens_user_idx    ON refresh_tokens (user_id, created_at DESC);
CREATE INDEX refresh_tokens_expiry_idx  ON refresh_tokens (expires_at) WHERE state = 'ACTIVE';

-- ---------------------------------------------------------------------------

CREATE TABLE login_attempts (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email      TEXT        NOT NULL,
    user_id    TEXT,
    succeeded  BOOLEAN     NOT NULL,
    failure_reason TEXT,
    ip_address INET,
    user_agent TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Powers per-account and per-IP lockout. Indexed on both because credential
-- stuffing spreads across accounts from few IPs, while a targeted attack does
-- the opposite.
CREATE INDEX login_attempts_email_idx ON login_attempts (lower(email), created_at DESC);
CREATE INDEX login_attempts_ip_idx    ON login_attempts (ip_address, created_at DESC);

CREATE TABLE password_reset_tokens (
    token_hash TEXT PRIMARY KEY,
    user_id    TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    used       BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Short. A reset link sitting in an inbox for a day is a standing key to
    -- the account.
    expires_at TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '30 minutes'
);

CREATE TABLE outbox (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    aggregate_type TEXT        NOT NULL,
    aggregate_id   TEXT        NOT NULL,
    event_id       TEXT        NOT NULL UNIQUE,
    event_type     TEXT        NOT NULL,
    topic          TEXT        NOT NULL,
    partition_key  TEXT        NOT NULL,
    payload        JSONB       NOT NULL,
    headers        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at   TIMESTAMPTZ,
    attempts       INT         NOT NULL DEFAULT 0,
    last_error     TEXT
);
CREATE INDEX outbox_unpublished_idx ON outbox (created_at) WHERE published_at IS NULL;
