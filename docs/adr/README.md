# Architecture Decision Records

Only decisions that were genuinely contested, or that will look wrong to
someone reading the code without the context. A record that says "we used
Postgres because it is a good database" is noise.

| # | Decision | Status |
|---|----------|--------|
| [0001](0001-model-before-implementing.md) | Model the saga exhaustively before writing it | Accepted |
| [0002](0002-orchestration-over-choreography.md) | Orchestration, not choreography | Accepted |
| [0003](0003-no-rollback-past-commit.md) | No compensation past the point of no return | Accepted |
| [0004](0004-five-languages.md) | Five languages across eleven services | Accepted |
| [0005](0005-database-per-service.md) | One Aurora cluster per service, not one with five schemas | Accepted |
| [0006](0006-paymob-merchant-order-id.md) | Use `merchant_order_id` as the Paymob idempotency mechanism | Accepted |
| [0007](0007-degrade-explicitly.md) | Degradation is a field in the contract, not an implementation detail | Accepted |
