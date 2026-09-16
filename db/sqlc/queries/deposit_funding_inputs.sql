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

-- name: CountDepositFundingInput :one
-- CountDepositFundingInput reports whether an outpoint is recorded as the
-- funding input of any boarding deposit. An own-wallet proceeds row arriving
-- after the deposit it funded uses this to discover that it must reverse its
-- own credit.
SELECT CAST(COUNT(*) AS BIGINT) AS input_count
FROM ledger_deposit_funding_inputs
WHERE input_hash = $1
  AND input_index = $2;
