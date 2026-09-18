# Register a custom receive policy

A client that watches an unfunded custom Taproot output needs permission to
query that output before it exists. `RegisterPolicyReceiveScript` registers
such a destination with the operator using the daemon's identity key. The
request contains the exact output script and its encoded Ark policy template.

The daemon reconstructs the output before signing. The indexer proof commits
to the output, the complete policy, the signing participant, the authenticated
mailbox principal, the operator identifier, the purpose, a nonce, and a short
proof lifetime. The policy is record 12 in the existing version-0 registration
TLV stream. The operator must reconstruct the same output and require the
signer to have an operator-backed settlement pair. Merely appearing in an
unrelated exit leaf does not establish ownership. The proof is domain-separated
from transaction signatures and cannot authorize a spend.

For a virtual hash-time-locked contract (vHTLC), either the sender or receiver
can prove participation without knowing the aggregate Taproot private key.
The operator key alone is not a recipient ownership proof.

## Persistence and lifetime

Registration upserts one `(principal, pkScript)` binding in the operator's
database. Repeating a request, losing its reply, or restarting the daemon does
not allocate another binding. The identity key and mailbox identity must remain
the same across restart.

The daemon requests 28 days of retention and reports that deadline. This stays
below the operator's 30-day cap. Proof signatures remain short-lived; proof
expiry does not remove the durable binding. A caller that still needs negative
observations after the retention deadline must register again. Errors during
registration, authentication, or lookup are inconclusive.

A funded output's persisted policy independently authorizes its participants to
query its metadata. Registration expiry therefore does not revoke recovery
access to indexed funds. Registration is not an admission deadline and cannot
prevent someone from funding a destination later. A negative query describes
its authoritative snapshot only; applications must retain their existing rules
for late funding and unresolved transfers.

## Compatibility

The daemon RPC is additive and requires `address:write` permission. An older
daemon returns `Unimplemented`. An operator that only understands direct
Taproot or standard VTXO proofs rejects a vHTLC registration; callers must
propagate that failure. Deploy operator support for policy ownership proofs
before applications depend on successful registration.
