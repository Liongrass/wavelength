package db

import (
	"bytes"
	"database/sql"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/stretchr/testify/require"
)

// TestOwnedWalletScriptStore verifies the registry answers ownership for a
// recorded script, answers false rather than erroring for one it has never
// seen, and treats a repeat registration as a no-op.
func TestOwnedWalletScriptStore(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	base := NewTestDB(t)
	store := &OwnedWalletScriptStore{
		TransactionExecutor: NewTransactionExecutor(
			base.BaseDB,
			func(tx *sql.Tx) *sqlc.Queries {
				return base.WithTx(tx)
			},
			btclog.Disabled,
		),
	}

	minted := bytes.Repeat([]byte{0x51}, 34)
	foreign := bytes.Repeat([]byte{0x52}, 34)

	// An unknown script is not an error: "not found" is the answer.
	owned, err := store.IsOwnedWalletScript(ctx, minted)
	require.NoError(t, err)
	require.False(t, owned)

	require.NoError(
		t, store.RecordOwnedWalletScript(
			ctx, minted, OwnedWalletScriptSourceReceive,
		),
	)

	owned, err = store.IsOwnedWalletScript(ctx, minted)
	require.NoError(t, err)
	require.True(t, owned)

	// Re-recording is a no-op, so a mint site can register without first
	// asking whether the script is already known.
	require.NoError(
		t, store.RecordOwnedWalletScript(
			ctx, minted, OwnedWalletScriptSourceSweep,
		),
	)

	owned, err = store.IsOwnedWalletScript(ctx, foreign)
	require.NoError(t, err)
	require.False(t, owned)
}
