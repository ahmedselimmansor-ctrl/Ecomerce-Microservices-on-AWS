# identity-service

Java 21 · Spring Boot 3.3 · port 8081 · PostgreSQL `identity`

Issues and rotates the credentials every other service trusts. Nothing else in the platform can
mint an access token, and nothing else calls this service to verify one.

---

## The two decisions that shape everything else

**Access tokens are verified locally, everywhere.** Every service checks the RS256 signature
against a JWKS it fetched from here and cached for five minutes. The alternative — an
introspection call per request — makes this service a synchronous dependency of literally every
request in the platform, so its bad afternoon becomes a total outage. The cost is that a revoked
token stays valid until it expires, which is why the TTL is **15 minutes** and not 15 hours.

**Refresh tokens rotate, with reuse detection.** Each refresh mints a new token and retires the
old one. A retired token coming back means either an attacker replaying a stolen one or a
legitimate client retrying after a lost response — and there is no way to tell which. Both are
treated as compromise: the whole family is revoked and the user signs in again. That is the
OAuth 2.1 recommendation, and the occasional spurious logout is the price of capping stolen-token
damage at a single use.

---

## Three things that look like bugs and are not

**Registration always returns 202 with the same body.** A 409 for a taken email turns an
unauthenticated endpoint into an account-enumeration oracle. A duplicate registration emails the
existing owner instead. Same reasoning for `POST /forgot-password`.

**An unknown email costs the same CPU as a wrong password.** `PasswordService.verifyAgainstDummy()`
runs a full Argon2id verification against a fixed hash when no account exists. Without it an
unknown email returns in microseconds and a wrong password takes ~50 ms, which is a reliable
enumeration oracle even when the response bodies are identical.

**A disabled account gets the same 401 as a wrong password.** Telling a stranger that an account
exists but is disabled is more than they should learn.

---

## Layout

| Path | What is in it |
|---|---|
| `auth/PasswordService` | Argon2id (19 MiB, t=2, p=1), k-anonymity breach check that **fails open**, 12-character minimum with no composition rules |
| `auth/AuthService` | Register and login. Per-account **and** per-IP lockout — credential stuffing spreads across accounts so per-account counting never trips; a targeted attack does the opposite |
| `auth/ProfileService` | Password change and reset. Only the SHA-256 of a reset token is stored |
| `auth/Totp` | RFC 6238, ±1 step window, constant-time comparison |
| `token/TokenService` | Issue, rotate, revoke. Reuse detection lives here |
| `token/AccessTokenVerifier` | Algorithm pinned to RS256 **before** the header is trusted |
| `token/KmsSigningKeyProvider` | Refuses to generate an in-memory key outside local/test; validates the KMS key spec at startup |
| `token/KmsJwsSigner` | Signs by KMS API call. Digests locally and sends `MessageType.DIGEST` — KMS caps a raw message at 4096 bytes and a JWT grows with its claims |
| `api/` | Controllers, the RFC 9457 envelope, the bearer filter |
| `event/OutboxRelay` | Transactional outbox → Kafka, `FOR UPDATE SKIP LOCKED` |

---

## Verification

There is no JDK in the development container, so `mvn verify` runs in CI and not locally. Two
things do run here, and both catch real errors:

```bash
python3 scripts/java-check.py services/identity-service/src
```

Resolves every type name and every call against a type this repository declares. It exists
because a controller once called `TokenService.describe(...)`, a method that was never written,
and nothing local said so. It is not a type checker — it has no type inference, so a call through
a local variable is invisible to it.

```bash
make sql-check
```

Applies the Flyway migrations to a throwaway Postgres and asserts what the schema must reject:
a second account differing only by capitalisation, a role outside the fixed set, two refresh
tokens sharing a hash, a token state the service cannot produce.

---

## Configuration

| Variable | Meaning |
|---|---|
| `SOUQ_JWT_KEY_SOURCE` | `local` generates an in-memory key; `kms` signs through AWS KMS and the private half never leaves it |
| `SOUQ_JWT_KMS_KEY_ID` | The signing key ARN. Required when the source is `kms`; validated at **startup** — a non-RSA key or one that cannot do PKCS#1 v1.5 fails the readiness probe rather than the first login |
| `SOUQ_ENV` | Anything but `local`/`test` makes a generated key a **startup failure** — every pod would sign differently and roughly `(n-1)/n` of verifications would fail |
| `SOUQ_JWT_ISSUER` / `SOUQ_JWT_AUDIENCE` | Must match what every other service checks |
| `souq.security.max-failed-logins-per-account` / `-per-ip` | 5 and 20 over a 15-minute window |

## Signing through KMS

`SigningKeyProvider` deals in a Nimbus `JWSSigner`, not a private key, and that indirection is what
makes the KMS path expressible at all — an interface returning `RSAPrivateKey` can only ever
describe a key this process holds.

The cost is a network round trip per token issue, roughly 10–20 ms. That is acceptable **because it
is on issue and never on verify**: every other service checks signatures locally against the cached
JWKS, so KMS is called once per login and once per refresh — at most every 15 minutes per active
session — not once per request.

RSA rather than EC, deliberately. KMS returns an ECDSA signature in DER while JWS wants the
concatenated `R||S` form, so an EC key needs a conversion step in the signing path. What KMS returns
for `RSASSA_PKCS1_V1_5_SHA_256` is byte-for-byte what JWS expects.

Rotation is a **timed procedure**, not a flag — AWS cannot rotate an asymmetric key in place, and
`enable_key_rotation` is silently ignored on `SIGN_VERIFY` keys. The JWKS cache TTL and the access
TTL together set the minimum safe window at about 25 minutes:
[`docs/runbooks/jwt-key-rotation.md`](../../docs/runbooks/jwt-key-rotation.md).

## Configuration notes

The access and refresh TTLs are **constants in `TokenService`, not configuration**. CONTRACTS §7
fixes them and every service caches JWKS on that basis; an operator who could widen the
revocation window with an environment variable would be doing it platform-wide, in one service,
with nothing to review.
