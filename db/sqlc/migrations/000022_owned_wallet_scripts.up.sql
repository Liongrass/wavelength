-- Durable registry of backing-wallet scripts this daemon minted, plus the
-- two UTXO classifications that the own-wallet accounting legs use.

-- owned_wallet_scripts records every pkScript the daemon handed out from its
-- own backing wallet. The client has no script-ownership oracle across its
-- three wallet backends -- LND, the lightweight wallet, and btcwallet each
-- answer "is this mine" differently or not at all -- so ownership is recorded
-- at the one moment the daemon knows for certain: when it mints the script.
-- A script that is absent means "not known to be ours", which is the
-- conservative answer: it books a destination as a genuine outflow rather
-- than as an internal transfer.
CREATE TABLE IF NOT EXISTS owned_wallet_scripts (
    -- pk_script is the raw output script, and its own identity.
    pk_script BLOB PRIMARY KEY,

    -- source names the mint site, so an operator reading the table can tell
    -- a receive address from a sweep or change destination.
    --
    -- The neighbouring owned_receive_scripts constrains its own source column
    -- with a foreign key into a lookup table. This column deliberately does
    -- not. That column is a discovery classification the OOR protocol reasons
    -- over, where an unrecognised value would change behaviour; this one is
    -- operator-facing provenance that nothing reads back, and every writer is
    -- a Go constant in this repository. A lookup table would buy a constraint
    -- on a value no code branches on, at the cost of a second table and a
    -- migration every time a new mint site appears.
    source TEXT NOT NULL,

    -- created_at is the Unix timestamp the script was recorded.
    created_at BIGINT NOT NULL
);

-- New UTXO audit classifications for the own-wallet proceeds lifecycle.
--
-- Three of them name the same fact: a UTXO whose value the ledger already
-- credited to wallet_balance at that outpoint. exit_proceeds is the sweep
-- output of a unilateral exit, leave_proceeds the on-chain output of a
-- cooperative leave paying a script this daemon minted, and recycled_change
-- the change of a partial spend of either. A 'created' row under one of them
-- is what lets a later boarding deposit recognise its own funding input and
-- reverse the credit instead of booking it twice.
--
-- deposit_funding is the other half of that pair: it marks the spend of such
-- a UTXO into a boarding address. The two rows are written by different
-- messages that can arrive in either order, and whichever commits second
-- books the reversing leg.
INSERT INTO utxo_classifications (classification) VALUES
    ('exit_proceeds'),
    ('leave_proceeds'),
    ('recycled_change'),
    ('deposit_funding')
ON CONFLICT DO NOTHING;
