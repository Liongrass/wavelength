-- Read-only projections used by the accounting invariant checker
-- (internal/cmd/tools/accounting). Every query here is a SELECT; the
-- checker never writes. Keep the classification policy that decides
-- which audit rows are expected to carry a ledger leg in Go next to
-- the handlers that apply it, so these queries stay policy-free and
-- return the raw rows the checker filters.

-- name: SumUnspentVTXOAmountsByStatus :many
-- SumUnspentVTXOAmountsByStatus totals the unspent VTXOs per status. Which
-- statuses count toward the ledger's vtxo_balance, and which are reconciling
-- items the check reports rather than fails on, is policy: it lives in Go
-- next to the handlers whose timing decides it, per this file's header.
SELECT status,
       CAST(COALESCE(SUM(amount), 0) AS BIGINT) AS amount_sat,
       CAST(COUNT(*) AS BIGINT) AS vtxo_count
FROM vtxos
WHERE spent = FALSE
GROUP BY status
ORDER BY status;

-- name: SumWalletUTXOLogByEventAndClassification :many
-- SumWalletUTXOLogByEventAndClassification returns the audit log totalled
-- per (event, classification) pair. The checker decides which pairs are
-- expected to have booked a ledger leg.
SELECT event,
       classified_as,
       CAST(COUNT(*) AS BIGINT) AS row_count,
       CAST(COALESCE(SUM(amount_sat), 0) AS BIGINT) AS total_sat
FROM wallet_utxo_log
GROUP BY event, classified_as
ORDER BY event, classified_as;

-- name: SumLedgerWalletLegs :many
-- SumLedgerWalletLegs totals the ledger legs that move value in and out of
-- wallet_balance, split by event type and by whether wallet_balance is the
-- debit or the credit side. The checker reconciles these against the audit
-- log totals above.
SELECT event_type,
       CASE
           WHEN debit_account = 'wallet_balance' THEN 'debit'
           ELSE 'credit'
       END AS wallet_side,
       CAST(COUNT(*) AS BIGINT) AS entry_count,
       CAST(COALESCE(SUM(amount_sat), 0) AS BIGINT) AS total_sat
FROM ledger_entries
WHERE debit_account = 'wallet_balance'
   OR credit_account = 'wallet_balance'
GROUP BY event_type, wallet_side
ORDER BY event_type, wallet_side;

-- name: ListLedgerRoundKeyDuplicates :many
-- ListLedgerRoundKeyDuplicates mirrors idx_client_ledger_idempotent_round:
-- no two round-keyed entries without an explicit idempotency key may share
-- (round_id, event_type, debit_account, credit_account).
SELECT round_id,
       event_type,
       debit_account,
       credit_account,
       CAST(COUNT(*) AS BIGINT) AS entry_count
FROM ledger_entries
WHERE round_id IS NOT NULL
  AND idempotency_key IS NULL
GROUP BY round_id, event_type, debit_account, credit_account
HAVING COUNT(*) > 1;

-- name: ListLedgerSessionKeyDuplicates :many
-- ListLedgerSessionKeyDuplicates mirrors
-- idx_client_ledger_idempotent_session.
SELECT session_id,
       event_type,
       debit_account,
       credit_account,
       CAST(COUNT(*) AS BIGINT) AS entry_count
FROM ledger_entries
WHERE session_id IS NOT NULL
GROUP BY session_id, event_type, debit_account, credit_account
HAVING COUNT(*) > 1;

-- name: ListLedgerIdempotencyKeyDuplicates :many
-- ListLedgerIdempotencyKeyDuplicates mirrors
-- idx_client_ledger_idempotent_key.
SELECT idempotency_key,
       event_type,
       debit_account,
       credit_account,
       CAST(COUNT(*) AS BIGINT) AS entry_count
FROM ledger_entries
WHERE idempotency_key IS NOT NULL
GROUP BY idempotency_key, event_type, debit_account, credit_account
HAVING COUNT(*) > 1;

-- name: ListKeyedLedgerEntries :many
-- ListKeyedLedgerEntries returns every entry carrying an idempotency key.
-- The checker parses the versioned "ledger:v1:<operation>:<leg>:" prefix in
-- Go, groups the legs of one outpoint-scoped operation, and compares their
-- amounts. Doing the prefix surgery in Go keeps the key encoding in one
-- place instead of restating it in a dialect-portable SQL expression.
SELECT entry_id, debit_account, credit_account, amount_sat,
       round_id, session_id, idempotency_key,
       event_type, description, created_at,
       chain_txid, chain_vout, confirmation_height, round_uuid
FROM ledger_entries
WHERE idempotency_key IS NOT NULL
ORDER BY entry_id;

-- name: ListWalletUTXOLogWithoutLedgerLeg :many
-- ListWalletUTXOLogWithoutLedgerLeg returns audit rows that have no ledger
-- entry at the same chain identity, event class AND amount. Matching the
-- amount too costs nothing and turns a pair that disagrees about value into
-- a finding rather than a silent pass. The checker filters the result down to
-- the classifications that are supposed to book a leg; audit-only
-- classifications legitimately appear here.
SELECT l.entry_id, l.outpoint_hash, l.outpoint_index, l.amount_sat,
       l.event, l.block_height, l.classified_as, l.created_at
FROM wallet_utxo_log AS l
WHERE NOT EXISTS (
    SELECT 1
    FROM ledger_entries AS le
    WHERE le.chain_txid = l.outpoint_hash
      AND le.chain_vout = l.outpoint_index
      AND (
          (l.event = 'created' AND le.event_type = 'wallet_utxo_created')
          OR (l.event = 'spent' AND le.event_type = 'wallet_utxo_spent')
      )
      AND le.amount_sat = l.amount_sat
)
ORDER BY l.entry_id;

-- name: ListWalletLedgerLegsWithoutAuditRow :many
-- ListWalletLedgerLegsWithoutAuditRow is the converse direction: a wallet
-- UTXO ledger leg whose audit row is missing, or present for a different
-- amount. Every such leg is a violation, since handleUTXOCreated and
-- handleUTXOSpent write both rows in one transaction from the same message.
SELECT entry_id, event_type, amount_sat, chain_txid, chain_vout,
       debit_account, credit_account, created_at
FROM ledger_entries
WHERE event_type IN ('wallet_utxo_created', 'wallet_utxo_spent')
  AND chain_txid IS NOT NULL
  AND chain_vout IS NOT NULL
  AND NOT EXISTS (
      SELECT 1
      FROM wallet_utxo_log AS l
      WHERE l.outpoint_hash = ledger_entries.chain_txid
        AND l.outpoint_index = ledger_entries.chain_vout
        AND l.event = CASE
            WHEN ledger_entries.event_type = 'wallet_utxo_created'
            THEN 'created'
            ELSE 'spent'
        END
        AND l.amount_sat = ledger_entries.amount_sat
  )
ORDER BY entry_id;

-- name: ListFlagFollowingExitSendLegs :many
-- ListFlagFollowingExitSendLegs finds the first known anomaly: an exit send
-- leg whose debit account followed the own-wallet destination flag, so it
-- reads "wallet_balance <- vtxo_balance" under a vtxo_sent event. Current
-- code always books the send leg on transfers_out and puts the own-wallet
-- movement in a separately keyed proceeds leg, so any row in this shape
-- predates that fix. twin_count reports whether the correctly-shaped send
-- leg for the same chain identity also exists.
SELECT le.entry_id, le.amount_sat, le.chain_txid, le.chain_vout,
       le.idempotency_key, le.created_at,
       CAST((
           SELECT COUNT(*)
           FROM ledger_entries AS twin
           WHERE twin.event_type = 'vtxo_sent'
             AND twin.debit_account = 'transfers_out'
             AND twin.credit_account = 'vtxo_balance'
             AND twin.chain_txid = le.chain_txid
             AND twin.chain_vout = le.chain_vout
       ) AS BIGINT) AS twin_count
FROM ledger_entries AS le
WHERE le.event_type = 'vtxo_sent'
  AND le.debit_account = 'wallet_balance'
  AND le.credit_account = 'vtxo_balance'
ORDER BY le.entry_id;

-- name: ListSelfChangeBookedAsTransfersIn :many
-- ListSelfChangeBookedAsTransfersIn finds the second known anomaly: an OOR
-- receive booked as transfers_in even though its session already carries an
-- outgoing vtxo_sent leg, which makes the receive the sender's own change.
-- The receive row carries no session_id of its own, so the session is
-- recovered through the OOR binding the history query uses.
SELECT r.entry_id, r.amount_sat, r.chain_txid, r.chain_vout,
       b.session_id, r.created_at
FROM ledger_entries AS r
JOIN oor_vtxo_bindings AS b
    ON b.outpoint_hash = r.chain_txid
   AND b.outpoint_index = r.chain_vout
   AND b.link_kind = 0
WHERE r.event_type = 'vtxo_received'
  AND r.debit_account = 'vtxo_balance'
  AND r.credit_account = 'transfers_in'
  AND EXISTS (
      SELECT 1
      FROM ledger_entries AS s
      WHERE s.session_id = b.session_id
        AND s.event_type = 'vtxo_sent'
        AND s.debit_account = 'transfers_out'
        AND s.credit_account = 'vtxo_balance'
  )
ORDER BY r.entry_id;

-- name: ListDepositLegsWithoutBoardingIntent :many
-- ListDepositLegsWithoutBoardingIntent finds the third known anomaly's first
-- direction: an audit row classified as a boarding deposit whose outpoint
-- has no boarding intent. Only the 'deposit' classification is checked;
-- change and sweep-return deposits legitimately have no intent.
SELECT l.entry_id, l.outpoint_hash, l.outpoint_index, l.amount_sat,
       l.block_height, l.created_at
FROM wallet_utxo_log AS l
WHERE l.event = 'created'
  AND l.classified_as = 'deposit'
  AND NOT EXISTS (
      SELECT 1
      FROM boarding_intents AS bi
      WHERE bi.outpoint_hash = l.outpoint_hash
        AND bi.outpoint_index = l.outpoint_index
  )
ORDER BY l.entry_id;

-- name: ListBoardingIntentsWithoutDepositLeg :many
-- ListBoardingIntentsWithoutDepositLeg is the converse: a confirmed boarding
-- intent with no wallet_utxo_created ledger leg at its outpoint. The deposit
-- leg is what balances the later boarding outflow, so a missing one drifts
-- wallet_balance negative by the intent amount.
--
-- Every status qualifies, not just 'confirmed': intents are written only once
-- the boarding UTXO has confirmed, so every row here was confirmed and the
-- later lifecycle statuses (adopted, swept) describe an intent that should
-- still carry its deposit leg. What does need excluding is the pre-ledger
-- era. boarding_intents arrives in migration 2 and the accounting schema in
-- migration 6, so an upgraded database holds intents from before any leg
-- could have been written. The scan therefore floors at the oldest audit row.
-- On a database with no audit rows the floor is NULL, the comparison is
-- never true, and nothing is reported -- the right answer for a ledger that
-- has recorded no wallet UTXO at all.
--
-- Zero heights are excluded from the floor. Producers write 0 for a height
-- they do not know yet (see blockHeight in round/actor.go), and one such row
-- would drag the minimum to 0, erase the floor and report every pre-ledger
-- intent as a violation. A sentinel is not evidence about when the ledger
-- era began, so it does not get a vote.
SELECT bi.outpoint_hash, bi.outpoint_index, bi.amount, bi.conf_height,
       bi.status
FROM boarding_intents AS bi
WHERE bi.conf_height >= (
      SELECT MIN(l.block_height) FROM wallet_utxo_log AS l
      WHERE l.block_height > 0
  )
  AND NOT EXISTS (
    SELECT 1
    FROM ledger_entries AS le
    WHERE le.event_type = 'wallet_utxo_created'
      AND le.chain_txid = bi.outpoint_hash
      AND le.chain_vout = bi.outpoint_index
)
ORDER BY bi.outpoint_hash, bi.outpoint_index;

-- name: ListLedgerEntriesForFingerprint :many
-- ListLedgerEntriesForFingerprint returns the whole journal in entry-ID
-- order so the checker can fingerprint it. The fingerprint binds to journal
-- content alone and names no database, host or file, which is what lets a
-- report prepared against a restored copy be compared with the production
-- daemon: identical journals produce an identical hash. It also means the
-- hash cannot tell an operator which of the two they are pointed at, so the
-- checker prints the entry count alongside it.
SELECT entry_id, debit_account, credit_account, amount_sat,
       round_id, session_id, idempotency_key,
       event_type, description, created_at,
       chain_txid, chain_vout, confirmation_height, round_uuid
FROM ledger_entries
ORDER BY entry_id;
