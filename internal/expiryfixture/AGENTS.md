# internal/expiryfixture

## Purpose

Builds real, signed VTXO ancestry for tests that exercise the incoming-VTXO
acceptance boundary. Acceptance code (expiry targeting, ancestry resolution,
incoming metadata, wallet recovery) refuses to trust unsigned or structurally
invalid paths, so these tests need genuine MuSig2-signed tree nodes rather than
hand-stubbed protos. This package produces exactly that, deterministically and
without standing up the round or OOR state machines.

## Key Types

- `Round(t, value, script, delay, height, tag)` — Returns a signed round-direct
  `arkrpc.VTXO` (one rooted `AncestryPath` with sweep key, sweep delay, and
  commitment height populated) plus the commitment transaction that funds it.
- `Merge(t, sameBatch)` — Returns a signed OOR-style `arkrpc.VTXO` that consumes
  two round leaves, plus the commitment transactions backing them. With
  `sameBatch` true both parents root into one confirmed commitment (testing
  distinct rooted paths inside a single batch); with it false the parents expire
  at different heights (testing fan-in across batches).

## Relationships

- **Depends on**: `arkrpc` (`VTXO`, `AncestryPath`, and the
  `AncestryPathFromTree` / `AncestryPathToTree` converters), `lib/tree` (`Node`,
  `Tree`, `ComputeFinalKey`, `VerifySigned`), `lib/arkscript`
  (`UnilateralCSVTimeoutTapLeaf`, `AnchorPkScript`), `lib/tx/psbtutil` (PSBT
  serialization for the merge package), `btcd` MuSig2 signing, and `testify`.
- **Depended on by** (tests only): `vtxo` (`expiry_target_test.go`,
  `incoming_ancestry_resolver_test.go`), `oor`
  (`incoming_metadata_query_test.go`, `receive_limits_test.go`), `waved`
  (`wallet_recovery_descriptor_test.go`, `wallet_recovery_oor_family_test.go`).
- **Sends** / **Receives**: none — a synchronous test helper, not an actor.

## Invariants

- **Fixtures are really signed.** `Round` runs a two-party MuSig2 session over
  the node sighash under the sweep tapscript tweak and asserts
  `Tree.VerifySigned` before returning. A test that stubs a path instead of
  calling this will be rejected by the acceptance code under test.
- **`tag` scopes the key material.** Cosigner keys derive from `{tag, 1}` (owner)
  and `{tag, 2}` (operator), and the commitment's input hash is `{tag}`. Two
  fixtures built with the same `tag` collide on commitment ID; callers that need
  independent lineages must pass distinct tags. `Merge` reserves tags `10` and
  `11` for its parents and `99` for the spending key.
- **Output is reproducible.** No randomness or wall-clock time is used, so the
  same arguments always produce the same txids — tests may assert on them.
- Every node carries the standard two outputs: the VTXO output and the
  `arkscript.AnchorPkScript` ephemeral anchor.
- The package takes `testing.TB` and calls `require`, so it is usable from tests
  and benchmarks but must never be linked into production paths.

## Deep Docs

- [internal/CLAUDE.md](../CLAUDE.md) — Parent internal package overview.
- [lib/tree/CLAUDE.md](../../lib/tree/CLAUDE.md) — The tree structures the
  fixtures build and sign.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
