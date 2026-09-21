# internal/actortest

## Purpose

Durable actor integration tests using real DB backends (SQLite, Postgres).
Verifies at-least-once delivery, exactly-once deduplication, FIFO ordering,
priority ordering, dead-letter and retry-ceiling invariants, DurableAsk/outbox-delivered
responses, concurrent senders/asks, recovery/restart scenarios, and atomic
state+outbox checkpointing.

## Key Test Infrastructure

- `testHarness` / `newTestHarness` — Central test scaffolding: sets up a
  per-test in-memory SQLite DB, `actor.ActorSystem`, and TX-aware actor
  delivery store, plus raw SQL queries for mailbox-row assertions; tests create
  their own `actor.OutboxPublisher` per case.
- `CounterBehavior` / `CounterMessage` (`IncrementMsg`, `DecrementMsg`,
  `GetCountMsg`, `ForwardMsg`) — Demo durable actor and TLV-coded messages
  used to drive the e2e scenarios.
- `eventuallyWithOutboxPublish` — Helper that actively triggers `OutboxPublisher.PublishPending()` on every polling iteration, making outbox delivery assertions robust under the race detector and CI scheduler pressure.
- `newLedgerActorForTest` (`ledger_e2e_test.go`) — Wires a real
  `ledger.LedgerActor` on the durable mailbox against the same SQLite DB, so
  ledger writes join the actor's fenced `Commit` transaction as in production.
- `nackingSendStore` (`ledger_session_lane_test.go`) — A `db.LedgerStoreDB`
  wrapper that fails the first outgoing OOR send leg, standing in for the
  transient storage failures the ledger's retry policy rides out. Failing once
  pushes that message's `available_at` into backoff so it sorts *behind* the
  later-enqueued receive, which is what makes the session-lane ordering
  guarantee observable rather than accidental.
- Timeout constants: `outboxForwardProcessingTimeout`, `outboxDeliveryTimeout`,
  `durableAskResponseTimeout` — all 30s, kept aligned since DurableAsk
  responses and forwards are also delivered through the outbox.

## Relationships

- **Depends on**: `baselib/actor`, `db` / `db/actordelivery` (real backends,
  not mocks), `ledger` (`LedgerActor` e2e coverage).
- **Depended on by**: nothing (test-only).

## Invariants

- **Ordering tests must force the adverse interleaving, not hope for it.**
  `TestOORSelfChangeSurvivesANackedSend` nacks the send so retry backoff moves
  it behind the receive in the mailbox's `(priority, available_at, created_at)`
  claim order. The assertion is that the session correlation key still keeps
  the receive from being claimed while its lane's earlier message is queued, so
  the receive is classified against a committed send leg and books as the
  sender's own change rather than as revenue from a counterparty. A test that
  merely enqueues in order proves nothing here.
- Ledger replay assertions use `require.Never`, not `require.Eventually`: the
  claim is that a redelivered event adds no rows, and only a negative
  assertion over time can show that.
