-- Two-factor enrolment.
--
-- V1 gave `users` an `mfa_secret` column and `mfa_enabled` flag, which is
-- enough to *verify* a code and not enough to *enrol*. Enrolment needs two
-- things V1 has nowhere to put:
--
--   1. a pending secret, issued but not yet proven to work
--   2. recovery codes
--
-- A new migration rather than an edit to V1: Flyway checksums applied
-- migrations, and `validate-on-migrate: true` exists to make an edit fail
-- loudly on every environment that already ran it.

-- ---------------------------------------------------------------------------
-- The pending secret.
--
-- Separate from users.mfa_secret on purpose. Writing the issued secret straight
-- into the live column would enable MFA the instant it is generated — locking
-- out every user whose authenticator app failed to save it, which is a support
-- call that ends in a human identity check. The secret only moves across once a
-- generated code has been verified.

ALTER TABLE users ADD COLUMN mfa_pending_secret TEXT;
ALTER TABLE users ADD COLUMN mfa_pending_since  TIMESTAMPTZ;

-- A pending secret that is never confirmed is dead weight and a small
-- liability. The sweeper clears them; this makes finding them cheap.
CREATE INDEX users_mfa_pending_idx ON users (mfa_pending_since)
    WHERE mfa_pending_secret IS NOT NULL;

ALTER TABLE users ADD CONSTRAINT mfa_pending_is_coherent
    CHECK ((mfa_pending_secret IS NULL) = (mfa_pending_since IS NULL));

-- Enabled implies a secret. Without this, a bug that sets the flag without the
-- secret locks the user out of their own account with no way back.
ALTER TABLE users ADD CONSTRAINT mfa_enabled_needs_a_secret
    CHECK (NOT mfa_enabled OR mfa_secret IS NOT NULL);

-- ---------------------------------------------------------------------------
-- Recovery codes.
--
-- Hashed, one row each, single use. Hashed because a database dump must not
-- hand over a bypass for every second factor in the system; one row each
-- because marking a single code used must not rewrite the others.
--
-- SHA-256 rather than Argon2id, deliberately and for the same reason as the
-- password-reset tokens: these are high-entropy random values, so there is no
-- dictionary to attack and a slow KDF buys nothing — while costing a table scan
-- on every verification, because Argon2id salts per row and cannot be looked up
-- by value.

CREATE TABLE mfa_recovery_codes (
    user_id    TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash  TEXT        NOT NULL,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (user_id, code_hash)
);

-- The lookup the verification path makes: "is this an unused code for this
-- user". Partial, because a used code is never searched for again.
CREATE INDEX mfa_recovery_unused_idx ON mfa_recovery_codes (user_id)
    WHERE used_at IS NULL;
