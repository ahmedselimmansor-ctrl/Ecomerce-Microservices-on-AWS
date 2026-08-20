# Runbook: rotating the JWT signing key

**Severity:** planned change, or page if the key is believed compromised
**Owner:** identity-service · **Key:** `alias/souq-prod-jwt-signing`

The private half never leaves KMS, so "the key leaked" is not a failure mode that applies here.
What does apply: a key needs replacing because someone with `kms:Sign` was compromised, because an
audit requires it, or because the key spec is changing.

**AWS cannot rotate an asymmetric KMS key in place.** `enable_key_rotation` is silently ignored on
`SIGN_VERIFY` keys, and it is deliberately absent from the Terraform for that reason. Rotation here
means creating a second key and moving traffic to it.

---

## The constraint that dictates the whole procedure

Every service verifies tokens **locally**, against a JWKS it caches for **five minutes**
(docs/CONTRACTS.md §7). Two consequences, and getting either wrong causes an outage:

1. **A new key must be published before it is used to sign.** Sign first and every verifier rejects
   every token for up to five minutes — a total, platform-wide auth outage.
2. **The old key must stay published until every token it signed has expired.** Access tokens live
   15 minutes. Remove the old key at cut-over and you invalidate every session in flight.

So the safe window is: publish → wait one JWKS TTL → switch signing → wait one access TTL plus one
JWKS TTL → retire. **Twenty-five minutes minimum, end to end.** Do not compress it.

---

## Procedure

### 1. Create the new key

```bash
aws kms create-key \
  --description "SOUQ JWT signing (rotation $(date +%Y-%m))" \
  --key-usage SIGN_VERIFY \
  --key-spec RSA_2048 \
  --query 'KeyMetadata.Arn' --output text
```

Do this in Terraform for a planned rotation — a key created by hand is a key nobody's `terraform
plan` knows about, and the next apply will propose deleting the alias out from under it.

Grant the identity-service IRSA role `kms:Sign` and `kms:GetPublicKey` on it. The role is
`module.eks.irsa_role_arns["identity-service"]`.

### 2. Publish it *before* signing with it

Add the new key to `retired` in `KmsSigningKeyProvider` — the field name is about how the key is
used, not about its age, and a key being published without being the signer is exactly this state.
Deploy, then confirm both keys are in the document:

```bash
curl -s https://auth.souq.dev/v1/.well-known/jwks.json | jq '.keys | length'
```

Expect **2**. If it says 1, stop — the next step will break every verification in the platform.

### 3. Wait out one JWKS TTL

Five minutes, plus a margin for a pod that started late. **Ten minutes.** Every verifier in the
platform must have both keys cached before anything is signed with the new one.

```promql
# Should be flat at zero throughout.
sum(rate(souq_jwt_verification_failures_total[1m]))
```

### 4. Switch the signer

Point `SOUQ_JWT_KMS_KEY_ID` at the new key and roll identity-service. The `kid` is derived from the
key id, so it changes automatically and every new token names the new key.

```bash
kubectl -n souq rollout status deploy/identity-service
```

Watch verification failures for five minutes. **If they rise, roll back the deployment** — the old
key is still published and still valid, so a rollback is clean and instant. That is the entire
reason for the ordering.

### 5. Wait out the old tokens

Access tokens live 15 minutes. Add a JWKS TTL for the verifier caches. **Twenty minutes** before the
old key can be removed from the document.

Refresh tokens are unaffected — they are opaque and stored hashed in Postgres, not signed.

### 6. Retire the old key

Remove it from `retired`, deploy, and confirm the document is back to one key. Then, and only then,
schedule the KMS key for deletion:

```bash
aws kms schedule-key-deletion --key-id <old-arn> --pending-window-in-days 30
```

Thirty days, not seven. Deleting this key makes every token it ever signed permanently
unverifiable, which matters if a dispute needs a historical token examined.

---

## If the key is compromised and you cannot wait

"Compromised" here means someone gained `kms:Sign` on the key, not that key material leaked — it
cannot. They could mint tokens for as long as they held the permission, and those tokens are
indistinguishable from real ones.

**Revoke the permission first.** It is instant and it stops the bleeding:

```bash
# Remove kms:Sign from the key policy, leaving kms:GetPublicKey in place.
aws kms put-key-policy --key-id <arn> --policy-name default --policy file://revoked.json
```

Signing now fails, so **identity-service cannot issue tokens** — logins fail and refreshes fail.
Existing access tokens keep working for up to 15 minutes. That is the deliberate trade: a total
login outage for the length of the rotation, against an attacker minting valid tokens.

Then run the procedure above at speed, and afterwards:

```sql
-- Every session, gone. Refresh tokens are not signed with this key, so they
-- survive the rotation on their own — which is not what you want after a
-- compromise.
UPDATE refresh_tokens SET state = 'REVOKED', revoked_reason = 'KEY_COMPROMISE'
 WHERE state = 'ACTIVE';
```

Expect every user to be signed out. Say so on the status page before you run it, not after.

---

## What will not work

**Removing the old key early to "clean up".** Every token it signed becomes unverifiable
immediately. There is no error that says this clearly — services return 401 and users see a login
loop.

**Rotating during a deploy of anything else.** The JWKS caches make this a timed procedure. A
concurrent rollout that restarts pods mid-window resets their caches at unpredictable moments.

**Assuming `enable_key_rotation = true` did anything.** It is accepted by the API and ignored for
`SIGN_VERIFY` keys. If you believe rotation is automatic, check the key's creation date:

```bash
aws kms describe-key --key-id alias/souq-prod-jwt-signing \
  --query 'KeyMetadata.{Created:CreationDate,Rotation:KeyUsage}'
```

---

## Related

- [`docs/CONTRACTS.md`](../CONTRACTS.md) §7 — token TTLs and the JWKS cache contract
- [`token-reuse.md`](token-reuse.md) — refresh-token reuse detection, a different failure
- `services/identity-service/src/main/java/dev/souq/identity/token/KmsSigningKeyProvider.java`
