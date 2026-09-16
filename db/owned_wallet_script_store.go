package db

import (
	"context"
	"database/sql"
	"time"

	"github.com/lightninglabs/wavelength/db/sqlc"
)

// Sources recorded alongside a minted backing-wallet script. They name the
// mint site so an operator reading owned_wallet_scripts can tell a receive
// address from a sweep or change destination.
const (
	// OwnedWalletScriptSourceReceive is a fresh receive address handed out
	// by the daemon's backing wallet.
	OwnedWalletScriptSourceReceive = "receive"

	// OwnedWalletScriptSourceSweep is a destination script minted for a
	// sweep or change output.
	OwnedWalletScriptSourceSweep = "sweep"
)

// OwnedWalletScriptStore is the durable registry of backing-wallet scripts
// this daemon minted. It answers the one question the wallet backends cannot:
// whether an on-chain destination belongs to us.
//
// The registry is written at mint time rather than queried from a backend
// because the three supported backends answer script ownership differently or
// not at all, and because a destination that is already being paid is too late
// to ask about. A script that is absent means "not known to be ours", which is
// the conservative answer for accounting: it books a payment as a genuine
// outflow rather than as an internal transfer between two accounts we own.
type OwnedWalletScriptStore struct {
	*TransactionExecutor[*sqlc.Queries]
}

// NewOwnedWalletScriptStore creates an OwnedWalletScriptStore from a Store.
func NewOwnedWalletScriptStore(store *Store) *OwnedWalletScriptStore {
	baseDB := store.BaseDB()

	txExec := NewTransactionExecutor(
		baseDB,
		func(tx *sql.Tx) *sqlc.Queries {
			return store.Queries().WithTx(tx)
		},
		store.log,
	)

	return &OwnedWalletScriptStore{
		TransactionExecutor: txExec,
	}
}

// RecordOwnedWalletScript registers a pkScript the daemon just minted from its
// own backing wallet. Re-recording the same script is a no-op, so a caller can
// register unconditionally at the mint site without first asking whether the
// script is already known.
func (s *OwnedWalletScriptStore) RecordOwnedWalletScript(ctx context.Context,
	pkScript []byte, source string) error {

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.UpsertOwnedWalletScript(
			ctx, sqlc.UpsertOwnedWalletScriptParams{
				PkScript:  pkScript,
				Source:    source,
				CreatedAt: time.Now().Unix(),
			},
		)
	})
}

// IsOwnedWalletScript reports whether a pkScript is in the registry. A script
// the registry has never seen returns false with no error: "not found" is an
// answer, not a failure.
func (s *OwnedWalletScriptStore) IsOwnedWalletScript(ctx context.Context,
	pkScript []byte) (bool, error) {

	var owned bool
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		count, err := q.CountOwnedWalletScript(ctx, pkScript)
		if err != nil {
			return err
		}
		owned = count > 0

		return nil
	})
	if err != nil {
		return false, err
	}

	return owned, nil
}
