-- Index from a boarding deposit's funding inputs back to the deposit, so the
-- ledger can reverse a credit it already booked for one of those inputs.
--
-- The problem this solves is ordering. Two independent producers describe the
-- same coin: the unroll actor or the round actor reports an own-wallet
-- proceeds UTXO, and the wallet reports the boarding deposit that spends it.
-- Both reach the ledger actor as fire-and-forget messages, so either can
-- commit first -- a sweep and a boarding tx in the same block, or a catch-up
-- scan surfacing both on one tick. The reversing leg needs both facts, and
-- whichever message commits second must be able to see what the first left
-- behind.
--
-- This table is the half the deposit leaves behind. It records the bare fact
-- "this outpoint funded that deposit" with no amount and no accounting
-- meaning, which is why it is not a wallet_utxo_log row: an audit row for a
-- funding input the client does not own would be a fiction, and every
-- reconciliation over that log would then have to special-case it.
CREATE TABLE IF NOT EXISTS ledger_deposit_funding_inputs (
    -- input_hash and input_index identify the previous outpoint the funding
    -- transaction spent.
    input_hash BLOB NOT NULL,
    input_index INTEGER NOT NULL,

    -- deposit_hash and deposit_index identify the boarding deposit that
    -- funding transaction created. One input can fund only one deposit in
    -- practice, but the deposit is part of the key so a transaction paying
    -- two boarding addresses records both without either overwriting the
    -- other.
    deposit_hash BLOB NOT NULL,
    deposit_index INTEGER NOT NULL,

    -- created_at is the Unix timestamp the row was recorded.
    created_at BIGINT NOT NULL,

    PRIMARY KEY (input_hash, input_index, deposit_hash, deposit_index)
);
