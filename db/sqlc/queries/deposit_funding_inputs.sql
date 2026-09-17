-- name: InsertDepositFundingInput :exec
-- InsertDepositFundingInput records one previous outpoint a boarding
-- deposit's funding transaction spent. Crash-replay safe: the durable ledger
-- mailbox replays unprocessed messages on startup, so a re-delivered deposit
-- re-inserts the same rows and must not fail.
INSERT INTO ledger_deposit_funding_inputs (
    input_hash, input_index, deposit_hash, deposit_index, created_at
) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (input_hash, input_index, deposit_hash, deposit_index)
DO NOTHING;

-- name: ListDepositsFundedByInput :many
-- ListDepositsFundedByInput returns the boarding deposits an outpoint is
-- recorded as funding. An own-wallet proceeds row arriving after the deposit
-- it funded uses this to discover that it must reverse its own credit, and to
-- find the deposit whose confirmation height stamps that reversal, so the leg
-- is byte-identical whichever message books it.
SELECT deposit_hash, deposit_index
FROM ledger_deposit_funding_inputs
WHERE input_hash = $1
  AND input_index = $2
ORDER BY deposit_hash, deposit_index;
