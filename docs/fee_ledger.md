# Fee Ledger and Double-Entry Accounting

This document is the authoritative reference for the client-side
fee ledger: what accounts exist, what every event class writes,
which subsystem emits each event, and how replay safety is
maintained. It complements the per-package docs:

- [ledger/CLAUDE.md](../ledger/CLAUDE.md) — the durable actor.
- [db/CLAUDE.md](../db/CLAUDE.md) — persistence adapter.
- [round/CLAUDE.md](../round/CLAUDE.md) — origin-routed emission.

## Why double-entry

Single-number "running balance" bookkeeping cannot distinguish
between "funds came in from outside" and "funds moved between
two places I already own," and it cannot distinguish gross flows
from net flows. Double-entry fixes both: every event writes two
rows (one debit leg, one credit leg) for the same amount, on two
different accounts. Sum of debits always equals sum of credits,
and per-account balances add up to meaningful gross figures for
tax reporting.

Two rules drive every decision in this package:

- **Every event produces exactly one debit + one credit, same
  amount, different accounts.** The DB enforces this via a
  `CHECK (debit_account != credit_account)` constraint on
  `ledger_entries`.
- **The account_type determines how a leg affects the balance.**
  Assets and expenses are "debit-normal": debiting them
  increases the number, crediting them decreases. Revenue,
  liability, and equity are "credit-normal": the mapping is
  inverted.

The mnemonic most bookkeepers use: **debit is left, credit is
right; assets and expenses grow to the left, everything else
grows to the right.**

## Chart of accounts

The chart of accounts is seeded by migration `000006_accounting.up.sql`:

| Account | Type | Meaning |
|---|---|---|
| `wallet_balance` | asset | On-chain wallet funds. |
| `vtxo_balance` | asset | Current VTXO holdings. |
| `fees_paid` | expense | Operator fees paid (boarding, refresh). |
| `onchain_fees` | expense | L1 chain/miner fees (exit costs). |
| `transfers_in` | revenue | Counterparty side of received VTXOs. |
| `transfers_out` | expense | Counterparty side of sent VTXOs. |
| `opening_balance` | equity | Source of funds for wallet UTXO deposits. |
| `wallet_clearing` | asset | Temporary clearing account for wallet sweeps. |

`transfers_in` and `transfers_out` stay as **separate** accounts
rather than a single "net transfers" account so tax-reporting
tooling can see gross send and gross receive flows
independently. Netting them would collapse useful information.

`opening_balance` is the equity counterparty for wallet UTXO
deposits. Without it, `SourceRoundBoarding` outflows
(crediting `wallet_balance`) would have no matching deposit leg
and the account would drift negative on every boarding.

## Event types

The `ledger_event_types` enum seeds these values:

- `wallet_utxo_created` — a wallet UTXO confirmed on-chain.
- `boarding_fee_paid` — operator fee for a boarding round.
- `refresh_fee_paid` — operator fee for a refresh round.
- `onchain_fee_paid` — L1 miner fee (exit cost).
- `boarding_sweep_fee_paid` — L1 chain cost for a boarding sweep.
- `vtxo_received` — the client received a VTXO (any source).
- `vtxo_sent` — the client sent or forfeited a VTXO.
- `wallet_utxo_spent` — a wallet UTXO spend that moves value into
  wallet clearing.
- `wallet_sweep_transfer` — external destination value paid by a
  wallet sweep.

The `event_type` column on `ledger_entries` references this
enum. Together with `debit_account` and `credit_account`, it
forms the tuple that the partial unique indexes key on for
replay dedup — see the [Replay safety](#replay-safety) section.

## Message-to-leg routing

| Message | Source value | Debit | Credit | Notes |
|---|---|---|---|---|
| `UTXOCreatedMsg` | deposit-like classifications | `wallet_balance` | `opening_balance` | Deposit leg. Written alongside the `wallet_utxo_log` audit row by `handleUTXOCreated`. |
| `BoardingSweepConfirmedMsg` | fee leg | `onchain_fees` | `wallet_clearing` | Miner fee + P2A anchor for a confirmed boarding sweep. One of the legs `handleBoardingSweepConfirmed` books atomically. |
| `BoardingSweepConfirmedMsg` | per input | `wallet_clearing` | `wallet_balance` | One leg + audit row per swept boarding input. |
| `BoardingSweepConfirmedMsg` | wallet-return dest | `wallet_balance` | `wallet_clearing` | Internal sweep return (`DestinationExternal=false`), with a `wallet_utxo_log` "created" row. |
| `BoardingSweepConfirmedMsg` | external dest | `transfers_out` | `wallet_clearing` | External sweep destination value (`DestinationExternal=true`). |
| `VTXOReceivedMsg` | `SourceRoundBoarding` | `vtxo_balance` | `wallet_balance` | Wallet → VTXO. Moves asset value across the on-chain / off-chain boundary. |
| `VTXOReceivedMsg` | `SourceRoundRefresh` | `vtxo_balance` | `transfers_out` | Refresh or directed-send self-change output. Paired with `VTXOSentMsg` for the gross forfeit; the two cancel on `transfers_out` so only the fee moves `vtxo_balance`. |
| `VTXOReceivedMsg` | `SourceRoundTransfer` | `vtxo_balance` | `transfers_in` | In-round receive from another participant's directed send. |
| `VTXOReceivedMsg` | `SourceOOR` | `vtxo_balance` | `transfers_in` | Out-of-round receive from another participant. |
| `VTXOReceivedMsg` | `SourceOOR` under a session with a booked `vtxo_sent` leg | `vtxo_balance` | `transfers_out` | The sender's own change from an outgoing OOR transfer, pushed back as a separate incoming session. The handler derives this from the ledger's own rows and books it as `SourceOORSelfChange`; it cancels the part of the companion `VTXOSentMsg` debit that never left. |
| `VTXOSentMsg` | (any) | `transfers_out` | `vtxo_balance` | One message per sent VTXO. Outpoint stamps an idempotency key so multi-VTXO rounds don't collapse. |
| `FeePaidMsg` | `FeeTypeBoarding` or `FeeTypeRefresh` | `fees_paid` | `vtxo_balance` | Operator fee for the round. |
| `FeePaidMsg` | `FeeTypeOnchainSweep` | `onchain_fees` | `wallet_clearing` | Boarding sweep chain cost, keyed by sweep txid. Retained for direct callers; the boarding sweep producer now folds this leg into `BoardingSweepConfirmedMsg`. |
| `ExitCostMsg` (send leg) | — | `transfers_out` | `vtxo_balance` | Net-of-fee value swept out of the VTXO. Booked the same way whatever the destination, so its dedup identity never depends on the flag. |
| `ExitCostMsg` (proceeds leg) | `DestinationOwnWallet=true` | `wallet_balance` | `transfers_out` | Net-of-fee value that landed in an output the client's own wallet controls. Cancels the send leg on `transfers_out`: an internal asset transfer, not an outflow. |
| `ExitCostMsg` (fee leg) | — | `onchain_fees` | `vtxo_balance` | Miner fee portion. |

Rejected sources fail with `ErrInvalidMessage`; a caller typo
or wire corruption dead-letters instead of silently
misclassifying the entry.

## Flow walkthroughs

Each section shows the exact ledger entries produced by a
given client-side flow, the per-account net effect, and which
subsystem emits each message.

### Wallet deposit

A confirmed wallet UTXO (external send into our boarding
address, or change from an earlier round-funding tx) passes
through the wallet actor's chain-source loop.

```
emitter: wallet.Ark.emitUTXOCreated (wallet/wallet.go)

Event 1 (wallet_utxo_created):
  debit  wallet_balance    += amount
  credit opening_balance   += amount
```

- `wallet_balance` gains the deposit (asset up).
- `opening_balance` records the equity source (credit-normal,
  so crediting increases it).

The accompanying `wallet_utxo_log` audit row lives in a
separate table (also seeded by migration `000006_accounting`);
it tracks the per-UTXO on-chain state machine and is out of
scope for double-entry accounting.

### Boarding round

Client's on-chain wallet input is consumed by a round and
materializes as an owned VTXO.

```
emitters:
  - wallet.Ark.emitUTXOCreated (for the deposit, before the round)
  - round.RoundClientActor.emitVTXOsReceived (VTXOOriginRoundBoarding branch)

Event 1 (wallet_utxo_created) — emitted when the wallet UTXO first confirmed:
  debit  wallet_balance    += gross
  credit opening_balance   += gross

Event 2 (vtxo_received, SourceRoundBoarding) — emitted when the round confirms:
  debit  vtxo_balance      += sealed  (gross − fee: the server's seal-time
  credit wallet_balance    += sealed   quote residual that lands in the leaf)

Event 3 (boarding_fee_paid) — if the round charged an operator fee:
  debit  fees_paid         += fee
  credit wallet_balance    += fee
```

The received leg books the SEALED leaf value (what the VTXO is
actually worth), so the fee leg credits `wallet_balance` — the
account the fee was truly paid from — completing the gross
wallet outflow without disturbing `vtxo_balance`.

The fee typing is round-level: a composed round mixing boarding
inputs with forfeited VTXOs emits ONE fee row typed boarding
(boarding takes precedence), so the refresh-carved share of that
round's fee is also credited from `wallet_balance`. The drift is
bounded by one round's fee and only affects the per-account audit
split, not any balance an RPC consumer reads today; a per-type
fee split proportioned by input source would eliminate it.

Per-account net effect:
- `opening_balance` ↑ by gross (tracks that these funds
  originated externally).
- `wallet_balance` unchanged (deposit in; sealed value + fee out).
- `vtxo_balance` ↑ by gross - fee (the sealed VTXO value).
- `fees_paid` ↑ by fee when a boarding operator fee exists.

The scenario test
`ledger.TestBoardingRoundNetsToOpeningBalanceAndVTXO` locks in
exactly this pattern.

### Refresh round

Client forfeits one or more VTXOs and receives new VTXOs of
approximately the same value. The only net economic change is
the operator fee; the gross amounts cancel.

```
emitter: round.RoundClientActor.emitVTXOsReceived (VTXOOriginRoundRefresh branch)
         round.RoundClientActor.emitRoundFee

Event 1 (vtxo_sent):
  debit  transfers_out     += sealed
  credit vtxo_balance      += sealed  (asset down)

Event 2 (vtxo_received, SourceRoundRefresh):
  debit  vtxo_balance      += sealed
  credit transfers_out     += sealed  (expense down, cancelling Event 1's debit)

Event 3 (refresh_fee_paid):
  debit  fees_paid         += fee
  credit vtxo_balance      += fee     (asset down by fee)
```

Both paired legs carry the same amount — the sealed value of the
replacement VTXO — so they cancel exactly and the fee leg is the
only real movement. A refresh fee is carved out of forfeited VTXO
value, so (unlike the boarding fee above) it credits
`vtxo_balance`.

Per-account net effect across all three events:
- `transfers_out` ↓ 0 (+gross from event 1 debit, −gross from event 2 credit).
- `vtxo_balance` ↓ by fee only.
- `fees_paid` ↑ by fee.
- `wallet_balance` untouched — the new VTXO was NOT funded by
  a wallet UTXO, so crediting `wallet_balance` would be wrong.

The three-leg cancellation is why `SourceRoundRefresh` exists
as a distinct source from `SourceRoundBoarding`. The scenario
test `ledger.TestRefreshRoundNetsToFeeOnVTXOBalance` locks in
each of the four invariants (transfers_out nets to zero,
wallet_balance untouched, vtxo_balance = −fee, fees_paid =
+fee).

Directed-send self-change uses the same source and emits the
same three-leg pattern, since economically it's a refresh that
also spawns a recipient VTXO on another participant's side.

### In-round participant transfer

One participant sends a VTXO to this client inside a round.
No wallet UTXO, no forfeit.

```
emitter (recipient side): round.RoundClientActor.emitVTXOsReceived
                          (VTXOOriginRoundTransfer branch)

Event 1 (vtxo_received, SourceRoundTransfer):
  debit  vtxo_balance      += amount
  credit transfers_in      += amount  (revenue up)
```

Per-account net effect:
- `vtxo_balance` ↑ by amount.
- `transfers_in` ↑ by amount (visible as gross receive for tax
  reporting).

The sender side emits a separate `VTXOSentMsg` with a round ID
and the recipient's amount (plus optional fee).

### OOR send and OOR receive

Structurally parallel to the in-round case, but keyed by
`SessionID` instead of `RoundID`.

```
emitter (sender side):   oor.sessionBehavior.queueVTXOSent
                         (oor/session_actor_handlers.go, on FinalizeAcceptedEvent)
emitter (recipient side): oor.sessionBehavior.queueVTXOsReceived
                          (in notifyMaterialized)

Sender event (vtxo_sent):
  debit  transfers_out     += Σ inputs
  credit vtxo_balance      += Σ inputs  (asset down)

Recipient event (vtxo_received, SourceOOR):
  debit  vtxo_balance      += amount
  credit transfers_in      += amount  (revenue up)
```

OOR rounds are currently fee-less on the wire (no `FeePaidMsg`
is emitted). If that changes, a new emission site would add a
third leg analogous to the refresh case.

A payment with change produces a third event. The sender books the
full input sum as `vtxo_sent`, and the operator pushes the sender's
own change output back as a separate incoming session under the same
session id:

```
Self-change event (vtxo_received, SourceOORSelfChange):
  debit  vtxo_balance      += change
  credit transfers_out     += change  (expense down)
```

This cancels the part of the sender's gross debit that never left, so
`transfers_out` ends at what the recipient actually got — the same
three-leg cancellation the refresh path uses, keyed by session instead
of round. `queueVTXOsReceived` sends every materialization as a plain
`SourceOOR` receive stamped with the session id, and
`handleVTXOReceived` tells the two cases apart from the ledger's own
rows: a `vtxo_sent` leg already booked under that session id means
this daemon dispatched the outgoing transfer, so the receive is its
change. The oracle is ordered by construction: the outgoing send leg
is enqueued in the outgoing session's commit, the operator only admits
the incoming self-transfer hint once that session is terminal, and the
durable mailbox delivers in enqueue order. It needs no caller
idempotency key and does not depend on the session row, which flips to
the incoming direction, so it holds for unkeyed sends and survives a
restart that re-drives materialization.

### Unilateral exit

Client unilaterally broadcasts a VTXO exit tree. The swept value
plus the miner fee both reduce `vtxo_balance`; the swept value
reappears on `wallet_balance` because the sweep pays the client's
own wallet.

```
emitter: unroll.behavior.emitExitCostIfCompleted, after the
         final sweep confirms

Send leg (vtxo_sent):
  debit  transfers_out    += (amount - fee)
  credit vtxo_balance     += (amount - fee)

Fee leg (onchain_fee_paid):
  debit  onchain_fees      += fee
  credit vtxo_balance      += fee

Proceeds leg (vtxo_sent), only when DestinationOwnWallet=true:
  debit  wallet_balance    += (amount - fee)
  credit transfers_out     += (amount - fee)
```

The unroll sweep pays a script the wallet handed out
(`buildSweepTx` takes the destination from
`SweepWallet.NewWalletPkScript`, and both exit-spend policies pay the
request's `DestinationPkScript` verbatim), so `emitExitCostIfCompleted`
stamps `DestinationOwnWallet=true` and the proceeds leg cancels the
send leg on `transfers_out`, landing the net value on
`wallet_balance`. The exit proceeds are an internal asset transfer:
the client still owns them, they just moved across the on-chain /
off-chain boundary. That sweep output pays a plain wallet script
rather than a boarding address, so no `UTXOCreatedMsg` producer books
it a second time.

`DestinationOwnWallet=false` writes no proceeds leg, so the send leg
stands as a real outflow for an exit whose destination is foreign,
and a payload written before the field existed decodes to exactly
that.

The send leg's accounts are fixed rather than following the flag
because the accounts are part of the dedup tuple in
`idx_client_ledger_idempotent_key`. A send leg that switched debit
account with the flag would land in a different tuple than its
pre-flag twin, and a resumed unroll job re-emitting the exit after
the upgrade would credit `vtxo_balance` twice. With the accounts
fixed, every redelivery of the outpoint dedups against the row it
wrote first, and the proceeds leg is the only row a flagged replay
adds. All three legs share the outpoint-derived identity under
distinct leg names (`send`, `fee`, `proceeds`), so a redelivered
`ExitCostMsg` resolves to a silent no-op.

The transaction history query lists the send leg as the exit and
excludes the proceeds leg, which shares the send leg's chain identity
and would otherwise list the same exit twice.

`fee` here is the **final sweep transaction's** miner fee
(`target output value − Σ sweep outputs`), not the cumulative
cost of the whole unilateral exit. Broadcasting the intermediate
tree transactions on the path to the leaf also burns miner fees,
and those are not yet captured by this leg. The exit cost
recorded today therefore understates the true on-chain cost of a
deep-tree exit; folding in the tree-broadcast fees is a deferred
item.

## Emission sites

| Subsystem | File | Function | Messages emitted |
|---|---|---|---|
| wallet | `wallet/wallet.go` | `emitUTXOCreated` | `UTXOCreatedMsg` on every confirmed wallet UTXO |
| wallet | `wallet/boarding_sweep_actor.go` | `commitFinalizedSweep` → `sweepConfirmedLedgerMsg` | one `BoardingSweepConfirmedMsg` per finalized boarding sweep |
| round | `round/actor.go` | `emitVTXOsReceived` → `emitOwnedVTXOLedgerEntry` | `VTXOReceivedMsg` (all sources), `VTXOSentMsg` (refresh pair) |
| round | `round/actor.go` | `emitRoundFee` | `FeePaidMsg` (`boarding` or `refresh`) |
| oor | `oor/session_actor_handlers.go` | `queueVTXOSent` / `queueVTXOsReceived` | `VTXOSentMsg` (session-keyed) / `VTXOReceivedMsg{Source=SourceOOR, SessionID}` |
| unroll | `unroll/actor.go` | `emitExitCostIfCompleted` | `ExitCostMsg` after final sweep confirmation |

The boarding sweep actor follows the same rule.
`commitFinalizedSweep` builds the sweep's message, then marks every
still-pending input spent and enqueues that message from inside
`BoardingSweepStore.FinalizeBoardingSweepInputs`'s write transaction
with the context that carries it. A refused enqueue rolls the
reconcile back, the sweep row stays unresolved, and the next start
re-drives its finalization, where the per-leg idempotency keys make
the replay a no-op.

The round actor does not Tell on emission. `emitVTXOsReceived` and
`emitRoundFee` stage every message under the round, and
`onRoundComplete` flushes the set from inside
`RoundStore.FinalizeRound`'s transaction with a context that carries
it, so the durable mailbox rows commit with the round row. A refused
enqueue fails finalization, the round stays active, and the next start
re-drives the confirmation; the idempotency keys then make the replay
a no-op. OOR sessions already follow the same rule through their
durable actor's commit transaction.

The round actor's emission path carries the most complexity
because a single round can mix boarding inputs, refresh inputs,
and remote directed-send recipients. The `VTXOOrigin` classifier
(stamped by the wallet at intent-composition time, threaded
through the FSM via `ClientVTXO.Origin`) is what lets
`emitVTXOsReceived` pick the right `Source` per VTXO without
having to re-derive it from commitment-tx inspection.

## Idempotency and replay safety

Durable actor delivery is at-least-once. Any ledger write must
be safe to replay. The schema enforces this via three partial
unique indexes on `ledger_entries`:

- `idx_client_ledger_idempotent_round` on
  `(round_id, event_type, debit_account, credit_account)
  WHERE round_id IS NOT NULL AND idempotency_key IS NULL`.
- `idx_client_ledger_idempotent_session` on
  `(session_id, event_type, debit_account, credit_account)
  WHERE session_id IS NOT NULL`.
- `idx_client_ledger_idempotent_key` on
  `(idempotency_key, event_type, debit_account, credit_account)
  WHERE idempotency_key IS NOT NULL`.

The `InsertClientLedgerEntry` query uses `ON CONFLICT DO
NOTHING`, so a replayed insert matching an existing row in any
of the three indexes is a silent no-op.

The three indexes together solve a subtlety: two events in the
same round that share `event_type + debit + credit` (for
example, two in-round receives, or the paired VTXOSent +
VTXOReceived emitted by refresh) would collide on the round
partial index. Stamping each row with an outpoint-derived
`idempotency_key` via `walletUTXOIdempotencyKey` (hash || LE
index, 36 bytes) routes those rows to the third index, which
does not share the collision.

Handlers that produce multi-leg events (`handleExitCost`,
refresh emission) rely on the durable actor's outer
transaction for atomicity: the whole `Receive` body runs inside
a `TxAwareDeliveryStore.ExecTx`, and
`db.TransactionExecutor.ExecTx` joins that tx via
`actor.TxFromContext`. Two `InsertLedgerEntry` calls from one
handler therefore commit together with the mailbox ack; a crash
between them rolls back both writes and the ack.

## Clock injection

`LedgerActor.clk` is a `clock.Clock` resolved at construction
from `ActorConfig.Clock` (falling back to
`clock.NewDefaultClock()`). Handlers stamp `CreatedAt` via
`a.clk.Now().Unix()` so tests can inject a deterministic clock
and assert on timestamps without wall-clock races. The
`opening_balance` deposit leg and every VTXO/fee leg share the
same clock source, so per-round event ordering is stable across
test runs.

## Deferred items

- **Non-boarding wallet spends.** Boarding sweep spends now
  write double-entry wallet-clearing legs, but other direct
  wallet spends still need a classification-specific ledger
  producer before they can affect `wallet_balance`.
- **Leave proceeds to an own-wallet destination.** A cooperative
  leave books its outflow on `transfers_out`, which is right for a
  foreign destination but understates the client's total when the
  leave paid the client's own wallet (the facade's default
  cooperative exit, which allocates a fresh backing-wallet address
  when the caller supplies none). The unroll exit already books
  this correctly via `ExitCostMsg.DestinationOwnWallet`, but no leave
  producer can assert ownership today: leave destinations arrive as
  caller-supplied addresses or pkScripts through the RPC surface, the
  client has no script-ownership oracle across its three wallet
  backends, and the one leave the wallet mints itself — the
  boarding-limit change output — pays a boarding script whose later
  `UTXOCreatedMsg` deposit leg already books the return, so flagging
  it would double-count. Closing this needs either an ownership query
  on the wallet backends or a destination flag on the leave RPC.
- **Recycled exit proceeds.** A unilateral exit books its proceeds
  into `wallet_balance` via `ExitCostMsg.DestinationOwnWallet`, and
  nothing ever debits them again. The exit output pays a plain wallet
  script rather than a boarding address, so it produces no boarding
  intent and no later `UTXOCreatedMsg`; the credit simply stands. If
  the user then spends those proceeds into a boarding address, that
  deposit's own `UTXOCreatedMsg` credits `wallet_balance` a second
  time for the same coins, and the total overstates the client by the
  recycled amount. Closing this needs a `wallet_clearing`-style
  close-out leg on the spend that funds the boarding address, the way
  the boarding sweep already clears its inputs.
- **Operator-swept VTXOs.** No producer books a VTXO whose batch
  the operator swept after expiry. This is deliberate for now:
  the server keeps honouring the claim, so the owner can still
  reclaim it by refreshing, and the asset is real until the
  operator's policy says otherwise. It does mean `vtxo_balance`
  can include value that a long-offline client may never
  recover.

## Related documents

- [ledger/CLAUDE.md](../ledger/CLAUDE.md) — caller contract,
  per-message rules, replay invariants.
- [db/CLAUDE.md](../db/CLAUDE.md) — schema and partial unique
  index details.
- [round/CLAUDE.md](../round/CLAUDE.md) — origin classification
  and per-origin emission dispatch.
- [wallet/CLAUDE.md](../wallet/CLAUDE.md) — deposit emission
  and intent-composition tagging.
- [docs/durable_actor_architecture.md](durable_actor_architecture.md)
  — outer-tx semantics for replay safety.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — system-wide package
  map.
