package actortest

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/db/actordelivery"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// errBusyWriter stands in for the transient storage failures the ledger's
// retry policy is built to ride out: a locked writer, a brief I/O error.
var errBusyWriter = errors.New("busy writer")

// nackingSendStore fails the first insert of an outgoing OOR send leg, which
// nacks that message and pushes its available_at into retry backoff. That is
// the whole point of the fixture: without a shared correlation key, the
// session's receive -- enqueued later but immediately claimable -- would
// overtake the send and be classified before the send's leg exists.
type nackingSendStore struct {
	*db.LedgerStoreDB

	failed atomic.Bool
}

// InsertLedgerEntry fails the first outgoing send leg and then behaves.
func (s *nackingSendStore) InsertLedgerEntry(ctx context.Context,
	entry ledger.LedgerEntry) error {

	isSend := entry.EventType == ledger.EventVTXOSent &&
		entry.DebitAccount == ledger.AccountTransfersOut

	if isSend && s.failed.CompareAndSwap(false, true) {
		return errBusyWriter
	}

	return s.LedgerStoreDB.InsertLedgerEntry(ctx, entry)
}

// TestOORSelfChangeSurvivesANackedSend proves the OOR self-change
// classification does not depend on the order the mailbox happens to claim a
// session's two messages in.
//
// The send is enqueued first but fails once, so its retry backoff moves it
// behind the receive in the mailbox's (priority, available_at, created_at)
// claim order. The two messages share a correlation key derived from the
// session id, so the claim SQL refuses to hand out the receive while its
// lane's earlier message is still queued. The receive is therefore still
// classified against a committed send leg, and books as the sender's own
// change coming back rather than as revenue from a counterparty.
func TestOORSelfChangeSurvivesANackedSend(t *testing.T) {
	t.Parallel()

	sqlDB := db.NewTestDB(t)
	clk := clock.NewDefaultClock()

	txStore, err := actordelivery.NewTxAwareDeliveryStoreFromDB(
		sqlDB.DB, sqlDB.Backend(), clk, btclog.Disabled,
	)
	require.NoError(t, err)

	base := &db.LedgerStoreDB{
		TransactionExecutor: db.NewTransactionExecutor(
			sqlDB.BaseDB,
			func(tx *sql.Tx) *sqlc.Queries {
				return sqlDB.WithTx(tx)
			},
			btclog.Disabled,
		),
	}
	store := &nackingSendStore{LedgerStoreDB: base}

	ledgerActor := ledger.NewLedgerActor(ledger.ActorConfig{
		Log:           fn.None[btclog.Logger](),
		DeliveryStore: txStore,
		LedgerStore:   store,
		Clock:         fn.Some[clock.Clock](clk),
	})
	require.NoError(t, ledgerActor.Start(t.Context()))

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()

		_ = ledgerActor.OnStop(ctx)
	})

	sessionID := [32]byte{0xd1}
	const (
		sentSat     = 50_000
		selfChange  = 20_000
		wantEntries = 2
	)

	require.NoError(
		t,
		ledgerActor.Ref().Tell(t.Context(), &ledger.VTXOSentMsg{
			SessionID: sessionID,
			AmountSat: sentSat,
		},
		),
	)
	require.NoError(
		t,
		ledgerActor.Ref().Tell(t.Context(), &ledger.VTXOReceivedMsg{
			SessionID:     sessionID,
			OutpointHash:  [32]byte{0xd2},
			OutpointIndex: 0,
			AmountSat:     selfChange,
			Source:        ledger.SourceOOR,
		},
		),
	)

	require.Eventually(t, func() bool {
		n, ok := tryCount(base)

		return ok && n == wantEntries
	}, 30*time.Second, 50*time.Millisecond)

	// The receive booked against transfers_out, cancelling the part of the
	// send that never left. Had it overtaken the send it would have
	// credited transfers_in instead, and nothing would repair that.
	requireBalance(t, base, ledger.AccountTransfersIn, 0)
	requireBalance(
		t, base, ledger.AccountTransfersOut, sentSat-selfChange,
	)
	requireBalance(
		t, base, ledger.AccountVTXOBalance, selfChange-sentSat,
	)

	require.True(t, store.failed.Load(), "the send must have been nacked")
}
