# Runbook: refresh token reuse spike

**Severity:** ticket · **Alert:** `RefreshTokenReuseSpike`

A retired refresh token was presented. Each occurrence revoked an entire session family, logging
that user out everywhere.

Occasional reuse is normal — a client that never received our response retries with the old
token. A **spike** is not.

## What it could be

| Cause | Signature | Action |
|---|---|---|
| A broken client release | Starts at a deploy; concentrated in one app version | Roll back the client |
| Credential theft | Spread across users, unusual IPs or geographies | Security incident |
| A race in the client | Two tabs refreshing simultaneously | Client-side mutex; not a security issue |
| Clock skew | Tokens rejected as expired, then retried | Check NTP on the client fleet |

## Triage

```sql
SELECT date_trunc('minute', created_at) AS minute,
       count(*) AS reuses,
       count(DISTINCT user_id) AS users,
       count(DISTINCT ip_address) AS ips
  FROM refresh_tokens
 WHERE state = 'REVOKED' AND revoked_reason = 'REUSE_DETECTED'
   AND created_at > now() - interval '1 hour'
 GROUP BY 1 ORDER BY 1 DESC;
```

**Many users, few IPs** → credential stuffing or a stolen token store. Security incident.
**Many users, many IPs, one app version** → a broken client. Roll it back.
**Few users, repeatedly** → those specific clients are racing. Usually two browser tabs.

## If it is theft

1. Revoke every session for the affected users:
   ```bash
   kubectl -n souq exec deploy/identity-service -- \
     curl -sS -X POST localhost:8081/internal/users/$USER_ID/revoke-all \
       -d '{"reason":"SUSPECTED_TOKEN_THEFT"}'
   ```
2. Force a password reset for them.
3. Check `login_attempts` for the same IPs against other accounts.
4. Consider temporarily lowering the auth rate limit at the WAF.

## Why we revoke the whole family

We cannot distinguish a thief replaying a stolen token from a user retrying after a network
failure. Guessing permissively hands an attacker a live session; guessing strictly logs someone
out once. This is the OAuth 2.1 recommendation and the occasional spurious logout is the price.

## Related
- [`services/identity-service/.../TokenService.java`](../../services/identity-service/src/main/java/dev/souq/identity/token/TokenService.java)
- [`docs/CONTRACTS.md`](../CONTRACTS.md) §7
