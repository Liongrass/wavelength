-- The ledger legs go first, while the audit rows that name them still exist.
-- Two of the four classifications book a leg of their own: recycled_change
-- writes a wallet_utxo_created credit, and deposit_funding writes the
-- wallet_utxo_spent reversal. Dropping the audit rows without these would
-- leave legs the invariant checker then reports as unpaired forever, so the
-- down file removes both halves of the pair rather than half of one.
DELETE FROM ledger_entries
WHERE event_type IN ('wallet_utxo_created', 'wallet_utxo_spent')
  AND EXISTS (
      SELECT 1
      FROM wallet_utxo_log AS l
      WHERE l.outpoint_hash = ledger_entries.chain_txid
        AND l.outpoint_index = ledger_entries.chain_vout
        AND l.event = CASE
            WHEN ledger_entries.event_type = 'wallet_utxo_created'
            THEN 'created'
            ELSE 'spent'
        END
        AND l.classified_as IN (
            'exit_proceeds', 'leave_proceeds', 'recycled_change',
            'deposit_funding'
        )
  );

-- Audit rows reference these classifications by foreign key, so they go
-- before the classifications themselves. Down-migrating past this version
-- discards the own-wallet proceeds audit trail, which is the same trade every
-- other accounting down file makes: the schema is restored exactly, and the
-- data that only the newer schema could hold does not survive.
DELETE FROM wallet_utxo_log
WHERE classified_as IN (
    'exit_proceeds', 'leave_proceeds', 'recycled_change', 'deposit_funding'
);

DELETE FROM utxo_classifications
WHERE classification IN (
    'exit_proceeds', 'leave_proceeds', 'recycled_change', 'deposit_funding'
);

DROP TABLE IF EXISTS owned_wallet_scripts;
