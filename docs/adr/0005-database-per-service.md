# 5. One Aurora cluster per service

**Status:** Accepted · **Date:** 2026-08

## Context

Five services need PostgreSQL. The cheap option is one Aurora cluster with five
databases, or one database with five schemas. The expensive option is five
clusters — roughly 5x the baseline cost before any of them does real work.

## Decision

Five clusters.

**Because the boundary has to be impossible to cross, not merely discouraged.**
A shared cluster means the boundary is a code review convention. Someone
eventually writes a join across `orders` and `catalog` because it is right
there and it makes a report easy, and from that moment the two services cannot
be deployed or migrated independently. Nobody notices until the first
incompatible migration.

Separate clusters also buy things that matter operationally:

- **Isolation of failure.** A catalogue reindex that saturates connections
  cannot starve checkout's pool.
- **Independent sizing.** `orders` scales to 32 ACUs with two replicas;
  `identity` sits at 8 with one. A shared cluster is sized for the sum of the
  peaks.
- **Independent recovery.** Restoring `reviews` to a point in time does not
  roll back payments.
- **Blast radius on credentials.** Each service's IRSA role reaches exactly one
  secret.

Locally, `docker-compose` creates five separate *databases* rather than five
schemas, for the same reason: a service that reaches across the boundary fails
on a developer's laptop rather than in staging.

## Consequences

**Cost.** Roughly 5x the Aurora baseline. Mitigated with Serverless v2 scaling
to 0.5 ACU, so an idle cluster is cheap — the trade is much better than the
sticker price suggests.

**No cross-service transactions, ever.** This is the point, and it is also why
the saga in [ADR-0002](0002-orchestration-over-choreography.md) exists. Every
cross-service consistency requirement has to be expressed as an eventually
consistent workflow with explicit compensation — which is harder, and which is
the reason it was model-checked.

**Reporting needs somewhere else to live.** Aurora MySQL (`analytics_ops`)
holds denormalised marts fed from Kafka. Deliberately a different engine, so a
heavy reporting query cannot be pointed at a transactional cluster by accident.

**Five sets of migrations.** Each service owns and runs its own. There is no
global schema version, and that is correct — a global version would be a
deployment coupling wearing a different hat.
