-- name: UpsertOwnedWalletScript :exec
-- UpsertOwnedWalletScript records a backing-wallet script the daemon minted.
-- Minting is idempotent from the registry's point of view: re-recording the
-- same script is a no-op, and the first source recorded wins so a script's
-- provenance never changes underneath an operator reading the table.
INSERT INTO owned_wallet_scripts (pk_script, source, created_at)
VALUES ($1, $2, $3)
ON CONFLICT (pk_script) DO NOTHING;

-- name: CountOwnedWalletScript :one
-- CountOwnedWalletScript reports whether a pkScript is in the registry.
-- Absent means "not known to be ours", which is the conservative answer.
SELECT CAST(COUNT(*) AS BIGINT) AS script_count
FROM owned_wallet_scripts
WHERE pk_script = $1;
