package db

import (
	"context"
	"database/sql"
	"errors"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/ledger"
)

// Compile-time check that UTXOAuditStoreDB implements
// ledger.UTXOAuditStore.
var _ ledger.UTXOAuditStore = (*UTXOAuditStoreDB)(nil)

// UTXOAuditStoreDB bridges the ledger.UTXOAuditStore
// interface to the sqlc-generated queries. This adapter converts
// UTXOAuditEntry to sqlc.InsertWalletUTXOLogParams and wraps
// all operations in ExecTx for transactional safety.
//
// Beyond the ledger.UTXOAuditStore interface,
// UTXOAuditStoreDB also provides query methods
// (ListUTXOAuditEntries, etc.) used by the daemon RPC layer.
type UTXOAuditStoreDB struct {
	*TransactionExecutor[*sqlc.Queries]
}

// NewUTXOAuditStoreDB creates a new UTXOAuditStoreDB from a
// Store.
func NewUTXOAuditStoreDB(store *Store) *UTXOAuditStoreDB {
	baseDB := store.BaseDB()

	txExec := NewTransactionExecutor(
		baseDB,
		func(tx *sql.Tx) *sqlc.Queries {
			return store.Queries().WithTx(tx)
		},
		store.log,
	)

	return &UTXOAuditStoreDB{
		TransactionExecutor: txExec,
	}
}

// InsertUTXOAuditEntry persists a UTXO audit log record within
// a database transaction.
func (s *UTXOAuditStoreDB) InsertUTXOAuditEntry(ctx context.Context,
	entry ledger.UTXOAuditEntry) error {

	return s.ExecTx(
		ctx, WriteTxOption(),
		func(qtx *sqlc.Queries) error {
			return qtx.InsertWalletUTXOLog(
				ctx, sqlc.InsertWalletUTXOLogParams{
					OutpointHash:  entry.OutpointHash,
					OutpointIndex: entry.OutpointIndex,
					AmountSat:     entry.AmountSat,
					Event:         entry.Event,
					BlockHeight:   entry.BlockHeight,
					ClassifiedAs:  entry.ClassifiedAs,
					CreatedAt:     entry.CreatedAt,
				},
			)
		},
	)
}

// ListUTXOAuditEntries returns a paginated list of UTXO audit
// entries ordered by creation time within a read transaction.
func (s *UTXOAuditStoreDB) ListUTXOAuditEntries(ctx context.Context, limit,
	offset int32) ([]sqlc.WalletUtxoLog, error) {

	var entries []sqlc.WalletUtxoLog
	err := s.ExecTx(
		ctx, ReadTxOption(),
		func(qtx *sqlc.Queries) error {
			var txErr error
			entries, txErr = qtx.ListWalletUTXOLog(
				ctx, sqlc.ListWalletUTXOLogParams{
					Limit:  limit,
					Offset: offset,
				},
			)

			return txErr
		},
	)

	return entries, err
}

// ListUTXOAuditEntriesByBlock returns all UTXO audit entries for
// a given block height within a read transaction.
func (s *UTXOAuditStoreDB) ListUTXOAuditEntriesByBlock(ctx context.Context,
	blockHeight int32) ([]sqlc.WalletUtxoLog, error) {

	var entries []sqlc.WalletUtxoLog
	err := s.ExecTx(
		ctx, ReadTxOption(),
		func(qtx *sqlc.Queries) error {
			var txErr error
			entries, txErr = qtx.ListWalletUTXOLogByBlock(
				ctx, blockHeight,
			)

			return txErr
		},
	)

	return entries, err
}

// ListUTXOAuditEntriesByClassification returns a paginated list
// of entries filtered by classification within a read
// transaction.
func (s *UTXOAuditStoreDB) ListUTXOAuditEntriesByClassification(
	ctx context.Context, classification string, limit, offset int32) (
	[]sqlc.WalletUtxoLog, error) {

	var entries []sqlc.WalletUtxoLog
	err := s.ExecTx(
		ctx, ReadTxOption(),
		func(qtx *sqlc.Queries) error {
			var txErr error
			entries, txErr = qtx.ListWalletUTXOLogByClassification(
				ctx,
				sqlc.ListWalletUTXOLogByClassificationParams{
					ClassifiedAs: classification,
					Limit:        limit,
					Offset:       offset,
				},
			)

			return txErr
		},
	)

	return entries, err
}

// CountUTXOAuditEntries returns the total number of UTXO audit
// entries within a read transaction.
func (s *UTXOAuditStoreDB) CountUTXOAuditEntries(ctx context.Context) (int64,
	error) {

	var count int64
	err := s.ExecTx(
		ctx, ReadTxOption(),
		func(qtx *sqlc.Queries) error {
			var txErr error
			count, txErr = qtx.CountWalletUTXOLog(ctx)

			return txErr
		},
	)

	return count, err
}

// LookupCreatedUTXO returns the 'created' audit row at an outpoint. The
// boolean is false when no such row exists, which is not an error: most
// outpoints the ledger asks about are not ours.
func (s *UTXOAuditStoreDB) LookupCreatedUTXO(ctx context.Context,
	outpoint wire.OutPoint) (ledger.UTXOAuditEntry, bool, error) {

	var (
		entry ledger.UTXOAuditEntry
		found bool
	)
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		row, err := q.GetWalletUTXOLogCreatedByOutpoint(
			ctx, sqlc.GetWalletUTXOLogCreatedByOutpointParams{
				OutpointHash:  outpoint.Hash[:],
				OutpointIndex: int32(outpoint.Index),
			},
		)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil

		case err != nil:
			return err
		}

		entry = ledger.UTXOAuditEntry{
			OutpointHash:  row.OutpointHash,
			OutpointIndex: row.OutpointIndex,
			AmountSat:     row.AmountSat,
			Event:         row.Event,
			BlockHeight:   row.BlockHeight,
			ClassifiedAs:  row.ClassifiedAs,
			CreatedAt:     row.CreatedAt,
		}
		found = true

		return nil
	})
	if err != nil {
		return ledger.UTXOAuditEntry{}, false, err
	}

	return entry, found, nil
}

// InsertDepositFundingInput records one previous outpoint a boarding
// deposit's funding transaction spent.
func (s *UTXOAuditStoreDB) InsertDepositFundingInput(ctx context.Context,
	input, deposit wire.OutPoint, createdAt int64) error {

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.InsertDepositFundingInput(
			ctx, sqlc.InsertDepositFundingInputParams{
				InputHash:    input.Hash[:],
				InputIndex:   int32(input.Index),
				DepositHash:  deposit.Hash[:],
				DepositIndex: int32(deposit.Index),
				CreatedAt:    createdAt,
			},
		)
	})
}

// IsDepositFundingInput reports whether an outpoint is recorded as the
// funding input of any boarding deposit.
func (s *UTXOAuditStoreDB) IsDepositFundingInput(ctx context.Context,
	outpoint wire.OutPoint) (bool, error) {

	var found bool
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		count, err := q.CountDepositFundingInput(
			ctx, sqlc.CountDepositFundingInputParams{
				InputHash:  outpoint.Hash[:],
				InputIndex: int32(outpoint.Index),
			},
		)
		if err != nil {
			return err
		}

		found = count > 0

		return nil
	})
	if err != nil {
		return false, err
	}

	return found, nil
}
