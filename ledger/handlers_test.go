package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// disabledLogger returns a no-op btclog logger.
func disabledLogger() btclog.Logger {
	return btclog.Disabled
}

// mockLedgerStore records all InsertLedgerEntry calls for
// assertion.
type mockLedgerStore struct {
	mu      sync.Mutex
	entries []LedgerEntry
}

func (m *mockLedgerStore) InsertLedgerEntry(_ context.Context,
	entry LedgerEntry) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.entries = append(m.entries, entry)

	return nil
}

func (m *mockLedgerStore) getEntries() []LedgerEntry {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]LedgerEntry{}, m.entries...)
}

// HasSessionEntry scans the recorded entries the way the session-keyed
// query does.
func (m *mockLedgerStore) HasSessionEntry(_ context.Context, sessionID [32]byte,
	eventType, debitAccount, creditAccount string) (bool, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	return hasSessionEntry(
		m.entries, sessionID, eventType, debitAccount, creditAccount,
	)
}

// hasSessionEntry mirrors GetClientLedgerEntryBySessionID over an in-memory
// slice: a match needs the session id plus the full event/account tuple.
func hasSessionEntry(entries []LedgerEntry, sessionID [32]byte, eventType,
	debitAccount, creditAccount string) (bool, error) {

	for _, entry := range entries {
		if !bytes.Equal(entry.SessionID, sessionID[:]) {
			continue
		}
		if entry.EventType != eventType ||
			entry.DebitAccount != debitAccount ||
			entry.CreditAccount != creditAccount {

			continue
		}

		return true, nil
	}

	return false, nil
}

// mockUTXOAuditStore records all InsertUTXOAuditEntry calls for
// assertion.
type mockUTXOAuditStore struct {
	mu      sync.Mutex
	entries []UTXOAuditEntry

	// fundingInputs records the outpoints reported as funding a boarding
	// deposit, so a test can pre-seed the deposit-arrived-first ordering.
	fundingInputs map[wire.OutPoint]struct{}
}

func (m *mockUTXOAuditStore) InsertUTXOAuditEntry(_ context.Context,
	entry UTXOAuditEntry) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.entries = append(m.entries, entry)

	return nil
}

// LookupCreatedUTXO returns the recorded 'created' row at an outpoint.
func (m *mockUTXOAuditStore) LookupCreatedUTXO(_ context.Context,
	outpoint wire.OutPoint) (UTXOAuditEntry, bool, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, entry := range m.entries {
		if entry.Event != "created" ||
			entry.OutpointIndex != int32(outpoint.Index) ||
			!bytes.Equal(entry.OutpointHash, outpoint.Hash[:]) {

			continue
		}

		return entry, true, nil
	}

	return UTXOAuditEntry{}, false, nil
}

// InsertDepositFundingInput records a deposit's funding input.
func (m *mockUTXOAuditStore) InsertDepositFundingInput(_ context.Context,
	input, _ wire.OutPoint, _ int64) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.fundingInputs == nil {
		m.fundingInputs = make(map[wire.OutPoint]struct{})
	}
	m.fundingInputs[input] = struct{}{}

	return nil
}

// IsDepositFundingInput reports whether an outpoint funded a deposit.
func (m *mockUTXOAuditStore) IsDepositFundingInput(_ context.Context,
	outpoint wire.OutPoint) (bool, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	_, ok := m.fundingInputs[outpoint]

	return ok, nil
}

func (m *mockUTXOAuditStore) getEntries() []UTXOAuditEntry {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]UTXOAuditEntry{}, m.entries...)
}

// newTestActor creates a LedgerActor with a mock store for
// testing handlers directly.
func newTestActor(t *testing.T) (*LedgerActor, *mockLedgerStore) {
	t.Helper()

	store := &mockLedgerStore{}

	a := &LedgerActor{
		cfg: ActorConfig{
			LedgerStore: store,
		},
		log: disabledLogger(),
		clk: clock.NewDefaultClock(),
	}

	return a, store
}

// newTestActorWithStore builds an actor bound to an explicit
// LedgerStore implementation. Used by tests that wire a custom
// store (e.g. the replay-idempotency dedup mock) instead of the
// default append-only mockLedgerStore.
func newTestActorWithStore(t *testing.T, store LedgerStore) *LedgerActor {
	t.Helper()

	return &LedgerActor{
		cfg: ActorConfig{
			LedgerStore: store,
		},
		log: disabledLogger(),
		clk: clock.NewDefaultClock(),
	}
}

// newTestActorWithAudit creates a LedgerActor with both a mock
// ledger store and a mock UTXO audit store. Returning both lets
// a test assert on the double-entry wallet deposit row that
// handleUTXOCreated writes alongside the wallet_utxo_log audit
// row, instead of only seeing the audit side.
func newTestActorWithAudit(t *testing.T) (*LedgerActor, *mockLedgerStore,
	*mockUTXOAuditStore) {

	t.Helper()

	ledgerStore := &mockLedgerStore{}
	auditStore := &mockUTXOAuditStore{}

	a := &LedgerActor{
		cfg: ActorConfig{
			LedgerStore:    ledgerStore,
			UTXOAuditStore: auditStore,
		},
		log: disabledLogger(),
		clk: clock.NewDefaultClock(),
	}

	return a, ledgerStore, auditStore
}

// fakeExec is a synchronous Exec[ledgerTx] for handler unit tests. It runs
// Read/Commit closures immediately against the actor's stores, with no real
// transaction or lease fence, so a handler's build-then-Commit flow can be
// exercised without standing up a durable mailbox.
type fakeExec struct {
	store ledgerTx
}

// Read runs fn against the actor's stores.
func (e fakeExec) Read(ctx context.Context,
	fn func(context.Context, ledgerTx) error) error {

	return fn(ctx, e.store)
}

// Stage runs fn against the actor's stores. The ledger handlers do not stage
// (they validate then Commit), but the Exec interface requires it.
func (e fakeExec) Stage(ctx context.Context,
	fn func(context.Context, ledgerTx) error) error {

	return fn(ctx, e.store)
}

// Commit runs fn against the actor's stores.
func (e fakeExec) Commit(ctx context.Context,
	fn func(context.Context, ledgerTx) error) error {

	return fn(ctx, e.store)
}

// run drives a message through the actor's Receive with a synchronous fake
// Exec and returns the handler error. The insert closures execute against the
// actor's mock stores, so the existing store-based assertions still hold while
// the validation/build work runs (as in production) outside any transaction.
func run(ctx context.Context, a *LedgerActor, msg LedgerMsg) error {
	ax := fakeExec{store: a.bindStores(ctx, nil)}

	return a.Receive(ctx, msg, ax).Err()
}

// TestHandleFeePaidBoarding verifies that a boarding fee is
// recorded with the correct accounts and event type.
func TestHandleFeePaidBoarding(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &FeePaidMsg{
		RoundID: [16]byte{
			1,
			2,
			3,
		},
		AmountSat:   1500,
		FeeType:     FeeTypeBoarding,
		BlockHeight: 800_000,
	}

	err := run(ctx, a, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountFeesPaid, entries[0].DebitAccount)

	// The boarding fee is paid from the on-chain wallet funds entering the
	// Ark layer: the boarding vtxo_received leg books the sealed (net of
	// fee) VTXO value, so the fee leg must credit wallet_balance to
	// complete the gross wallet outflow.
	require.Equal(t, AccountWalletBalance,
		entries[0].CreditAccount)
	require.Equal(t, int64(1500), entries[0].AmountSat)
	require.Equal(t, EventBoardingFeePaid,
		entries[0].EventType)
}

// TestHandleFeePaidRefresh verifies that a refresh fee is
// recorded with the correct event type.
func TestHandleFeePaidRefresh(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &FeePaidMsg{
		RoundID: [16]byte{
			4,
			5,
			6,
		},
		AmountSat:   750,
		FeeType:     FeeTypeRefresh,
		BlockHeight: 800_100,
	}

	err := run(ctx, a, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, EventRefreshFeePaid,
		entries[0].EventType)
}

// TestHandleFeePaidOnchainSweep verifies that a boarding-sweep chain cost
// is booked against onchain_fees / wallet_clearing with the
// boarding_sweep_fee_paid event type, and that an empty RoundID is
// accepted alongside a sweep-txid IdempotencyKey.
func TestHandleFeePaidOnchainSweep(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	sweepTxid := [32]byte{
		0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00, 0x11,
		0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99,
		0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00, 0x11,
		0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99,
	}
	msg := &FeePaidMsg{
		AmountSat:      444,
		FeeType:        FeeTypeOnchainSweep,
		BlockHeight:    800_650,
		IdempotencyKey: append([]byte(nil), sweepTxid[:]...),
	}

	err := run(ctx, a, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountOnchainFees, entries[0].DebitAccount)
	require.Equal(t, AccountWalletClearing, entries[0].CreditAccount)
	require.Equal(t, int64(444), entries[0].AmountSat)
	require.Equal(t, EventBoardingSweepFeePaid, entries[0].EventType)

	// Onchain-sweep fees do NOT carry a RoundID; the dedup key is the
	// sweep txid plumbed via IdempotencyKey, so the
	// idx_client_ledger_idempotent_key partial unique index handles
	// replay safety.
	require.Nil(
		t, entries[0].RoundID,
		"onchain-sweep fees must not carry a RoundID",
	)
	require.Equal(t, sweepTxid[:], entries[0].IdempotencyKey)
}

// TestHandleFeePaidOnchainSweepRejectsZeroAmount verifies that the same
// non-positive-amount guard the operator-fee path uses also fires for
// onchain-sweep fees.
func TestHandleFeePaidOnchainSweepRejectsZeroAmount(t *testing.T) {
	t.Parallel()

	a, _ := newTestActor(t)
	ctx := t.Context()

	msg := &FeePaidMsg{
		AmountSat:   0,
		FeeType:     FeeTypeOnchainSweep,
		BlockHeight: 800_700,
	}

	err := run(ctx, a, msg)
	require.ErrorIs(t, err, ErrInvalidMessage)
}

// TestHandleFeePaidOnchainSweepRejectsBadIdempotencyKey verifies that
// sweep fee rows cannot bypass every ledger idempotency index. Onchain
// sweep fees have no RoundID, so the sweep txid key is mandatory.
func TestHandleFeePaidOnchainSweepRejectsBadIdempotencyKey(t *testing.T) {
	t.Parallel()

	for _, key := range [][]byte{
		nil,
		{},
		{
			0x01,
			0x02,
		},
		make([]byte, chainhash.HashSize+1),
	} {
		t.Run(fmt.Sprintf("len_%d", len(key)), func(t *testing.T) {
			t.Parallel()

			a, store := newTestActor(t)
			ctx := t.Context()

			msg := &FeePaidMsg{
				AmountSat:      1_000,
				FeeType:        FeeTypeOnchainSweep,
				BlockHeight:    800_700,
				IdempotencyKey: key,
			}

			err := run(ctx, a, msg)
			require.ErrorIs(t, err, ErrInvalidMessage)
			require.Empty(t, store.getEntries())
		})
	}
}

// TestHandleVTXOReceivedRoundBoarding verifies that a boarding
// or refresh receive is recorded with wallet_balance ->
// vtxo_balance (own on-chain funds converted to VTXO balance).
func TestHandleVTXOReceivedRoundBoarding(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &VTXOReceivedMsg{
		OutpointHash: [32]byte{
			0xaa,
			0xbb,
		},
		OutpointIndex: 0,
		AmountSat:     50_000,
		Source:        SourceRoundBoarding,
		RoundID: [16]byte{
			7,
			8,
			9,
		},
	}

	err := run(ctx, a, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountVTXOBalance,
		entries[0].DebitAccount)
	require.Equal(t, AccountWalletBalance,
		entries[0].CreditAccount)
	require.Equal(t, int64(50_000), entries[0].AmountSat)
	require.Equal(t, EventVTXOReceived,
		entries[0].EventType)

	// The VTXO outpoint must land on the row's structured chain
	// fields too. Issue #504: without these the wallet's onchain
	// view renders a "round"-kind row with an empty txid and the
	// outpoint only available by parsing the description blob.
	require.Equal(
		t, msg.OutpointHash[:], entries[0].ChainTxid,
		"chain_txid must carry the VTXO outpoint hash",
	)
	require.NotNil(t, entries[0].ChainVout)
	require.Equal(
		t, int32(msg.OutpointIndex), *entries[0].ChainVout,
		"chain_vout must carry the VTXO outpoint index",
	)
}

// TestHandleVTXOReceivedRoundTransfer verifies that an in-round
// participant-to-participant VTXO receive is recorded the same
// way as an OOR receive: transfers_in -> vtxo_balance.
// TestHandleVTXOReceivedOORClassifiesSelfChangeBySession proves the ledger
// derives the self-change booking from its own rows: a SourceOOR receive
// under a session that already carries an outgoing send leg credits
// transfers_out, while the same receive under an unknown session credits
// transfers_in. No caller key or session-row state is consulted.
func TestHandleVTXOReceivedOORClassifiesSelfChangeBySession(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	sent := &VTXOSentMsg{
		SessionID: [32]byte{
			0x77,
		},
		AmountSat: 50_000,
	}
	require.NoError(t, run(ctx, a, sent))

	change := &VTXOReceivedMsg{
		OutpointHash: [32]byte{
			0x78,
		},
		OutpointIndex: 1,
		AmountSat:     20_000,
		Source:        SourceOOR,
		SessionID:     sent.SessionID,
	}
	require.NoError(t, run(ctx, a, change))

	foreign := &VTXOReceivedMsg{
		OutpointHash: [32]byte{
			0x79,
		},
		OutpointIndex: 0,
		AmountSat:     30_000,
		Source:        SourceOOR,
		SessionID: [32]byte{
			0x7a,
		},
	}
	require.NoError(t, run(ctx, a, foreign))

	entries := store.getEntries()
	require.Len(t, entries, 3)

	// The change cancels on transfers_out and says so in its row.
	require.Equal(t, AccountVTXOBalance, entries[1].DebitAccount)
	require.Equal(t, AccountTransfersOut, entries[1].CreditAccount)
	require.Contains(t, entries[1].Description, SourceOORSelfChange)

	// The foreign receive is a gross inflow.
	require.Equal(t, AccountVTXOBalance, entries[2].DebitAccount)
	require.Equal(t, AccountTransfersIn, entries[2].CreditAccount)

	// transfers_out nets to what actually left; transfers_in carries
	// only the foreign receive.
	balances := make(map[string]int64)
	for _, entry := range entries {
		balances[entry.DebitAccount] += entry.AmountSat
		balances[entry.CreditAccount] -= entry.AmountSat
	}
	require.Equal(t, int64(30_000), balances[AccountTransfersOut])
	require.Equal(t, int64(-30_000), balances[AccountTransfersIn])
}

// TestVTXOReceivedMsgSessionIDRoundTrips proves the session id survives the
// durable mailbox codec and that a payload written before the field existed
// decodes to the zero session, which books as a plain receive.
func TestVTXOReceivedMsgSessionIDRoundTrips(t *testing.T) {
	t.Parallel()

	msg := &VTXOReceivedMsg{
		OutpointHash: [32]byte{
			0x5f,
		},
		OutpointIndex: 2,
		AmountSat:     7_000,
		Source:        SourceOOR,
		SessionID: [32]byte{
			0x60,
		},
	}

	var buf bytes.Buffer
	require.NoError(t, msg.Encode(&buf))

	var decoded VTXOReceivedMsg
	require.NoError(t, decoded.Decode(bytes.NewReader(buf.Bytes())))
	require.Equal(t, msg.SessionID, decoded.SessionID)
	require.Equal(t, msg.Source, decoded.Source)

	var (
		outpointHash  = bytes.Repeat([]byte{0x5f}, 32)
		outpointIndex = uint32(2)
		amountSat     = uint64(7_000)
		source        = []byte(SourceOOR)
		roundID       = make([]byte, 16)
	)
	legacyStream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			vtxoRecvOutpointHashType, &outpointHash,
		),
		tlv.MakePrimitiveRecord(
			vtxoRecvOutpointIndexType, &outpointIndex,
		),
		tlv.MakePrimitiveRecord(vtxoRecvAmountSatType, &amountSat),
		tlv.MakePrimitiveRecord(vtxoRecvSourceType, &source),
		tlv.MakePrimitiveRecord(vtxoRecvRoundIDType, &roundID),
	)
	require.NoError(t, err)

	var legacyBuf bytes.Buffer
	require.NoError(t, legacyStream.Encode(&legacyBuf))

	var legacy VTXOReceivedMsg
	require.NoError(t, legacy.Decode(bytes.NewReader(legacyBuf.Bytes())))
	require.Equal(t, [32]byte{}, legacy.SessionID)
	require.Equal(t, int64(7_000), legacy.AmountSat)
}

// TestHandleVTXOReceivedOORSelfChange proves the sender's own OOR change
// cancels on transfers_out rather than being booked as a gross receive: the
// outgoing session already debited transfers_out for the whole input sum, so
// crediting transfers_in here would inflate both gross directions by the
// change amount even though vtxo_balance nets correctly either way.
func TestHandleVTXOReceivedOORSelfChange(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &VTXOReceivedMsg{
		OutpointHash: [32]byte{
			0x4d,
		},
		OutpointIndex: 1,
		AmountSat:     12_000,
		Source:        SourceOORSelfChange,
	}

	require.NoError(t, run(ctx, a, msg))

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountVTXOBalance, entries[0].DebitAccount)
	require.Equal(t, AccountTransfersOut, entries[0].CreditAccount)
	require.Equal(t, int64(12_000), entries[0].AmountSat)
	require.Equal(t, EventVTXOReceived, entries[0].EventType)

	// The key is the per-outpoint one every receive uses, so the change
	// leg dedups on redelivery exactly as a foreign receive does.
	require.Equal(
		t,
		walletUTXOIdempotencyKey(msg.OutpointHash, msg.OutpointIndex),
		entries[0].IdempotencyKey,
	)
}

func TestHandleVTXOReceivedRoundTransfer(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &VTXOReceivedMsg{
		OutpointHash: [32]byte{
			0xab,
			0xcd,
		},
		OutpointIndex: 0,
		AmountSat:     30_000,
		Source:        SourceRoundTransfer,
		RoundID: [16]byte{
			13,
			14,
			15,
		},
	}

	err := run(ctx, a, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountVTXOBalance,
		entries[0].DebitAccount)
	require.Equal(t, AccountTransfersIn,
		entries[0].CreditAccount)
}

// TestHandleVTXOReceivedRoundRefresh verifies that a refresh
// (or directed-send self-change) receive is booked vtxo_balance
// -> transfers_out, so the paired VTXOSentMsg (gross forfeit)
// and this leg cancel on transfers_out and the net effect on
// vtxo_balance is -fee once the FeePaidMsg lands separately.
// Crediting transfers_out instead of wallet_balance prevents
// wallet_balance from drifting on a flow that never touches the
// on-chain wallet.
func TestHandleVTXOReceivedRoundRefresh(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &VTXOReceivedMsg{
		OutpointHash: [32]byte{
			0xef,
			0x01,
		},
		OutpointIndex: 2,
		AmountSat:     40_000,
		Source:        SourceRoundRefresh,
		RoundID: [16]byte{
			16,
			17,
			18,
		},
	}

	err := run(ctx, a, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountVTXOBalance,
		entries[0].DebitAccount)
	require.Equal(t, AccountTransfersOut,
		entries[0].CreditAccount)
	require.Equal(t, int64(40_000), entries[0].AmountSat)
	require.Equal(t, EventVTXOReceived,
		entries[0].EventType)
}

// TestRefreshRoundNetsToFeeOnVTXOBalance is a scenario-level test
// that runs the three messages a refresh round emits
// (VTXOSent(gross) + VTXOReceived(SourceRoundRefresh, gross) +
// FeePaidMsg(refresh, fee)) against a shared mock store and asserts
// the running balances match the intended accounting model:
//   - transfers_out: net zero (debit from VTXOSent cancels credit
//     from SourceRoundRefresh).
//   - wallet_balance: untouched.
//   - vtxo_balance: down by exactly the fee (two offsetting legs
//     plus one fee credit).
//   - fees_paid: up by the fee.
//
// A regression here would indicate the refresh-handling invariant
// was broken in a later refactor.
func TestRefreshRoundNetsToFeeOnVTXOBalance(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	roundID := [16]byte{0xaa, 0xbb, 0xcc}
	const gross int64 = 100_000
	const fee int64 = 750

	// Leg 1: forfeit of the old VTXO shows as a VTXOSent with
	// RoundID set.
	require.NoError(
		t,
		run(
			ctx, a, &VTXOSentMsg{
				RoundID:   roundID,
				AmountSat: gross,
			},
		),
	)

	// Leg 2: new VTXO materializes with Source=SourceRoundRefresh.
	require.NoError(
		t,
		run(
			ctx, a, &VTXOReceivedMsg{
				OutpointHash:  [32]byte{0x01},
				OutpointIndex: 0,
				AmountSat:     gross,
				Source:        SourceRoundRefresh,
				RoundID:       roundID,
			},
		),
	)

	// Leg 3: the operator fee for the refresh round.
	require.NoError(
		t,
		run(
			ctx, a, &FeePaidMsg{
				RoundID:     roundID,
				AmountSat:   fee,
				FeeType:     FeeTypeRefresh,
				BlockHeight: 800_000,
			},
		),
	)

	entries := store.getEntries()
	require.Len(t, entries, 3)

	// Compute the running account balances (debit - credit per
	// account) across the three entries, matching what the DB
	// GetAccountBalance query would return.
	balances := map[string]int64{}
	for _, e := range entries {
		balances[e.DebitAccount] += e.AmountSat
		balances[e.CreditAccount] -= e.AmountSat
	}

	require.Equal(
		t, int64(0), balances[AccountTransfersOut],
		"forfeit+refresh legs must cancel on transfers_out",
	)
	require.Equal(
		t, int64(0), balances[AccountWalletBalance],
		"refresh must not touch wallet_balance",
	)
	require.Equal(
		t, -fee, balances[AccountVTXOBalance],
		"vtxo_balance must drop by exactly the operator fee",
	)
	require.Equal(
		t, fee, balances[AccountFeesPaid],
		"fees_paid must rise by exactly the operator fee",
	)
}

// TestHandleVTXOReceivedOOR verifies that an OOR-sourced VTXO
// received is recorded with vtxo_balance -> transfers_in.
func TestHandleVTXOReceivedOOR(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &VTXOReceivedMsg{
		OutpointHash: [32]byte{
			0xcc,
			0xdd,
		},
		OutpointIndex: 1,
		AmountSat:     25_000,
		Source:        SourceOOR,
		RoundID: [16]byte{
			10,
			11,
			12,
		},
	}

	err := run(ctx, a, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountVTXOBalance,
		entries[0].DebitAccount)
	require.Equal(t, AccountTransfersIn,
		entries[0].CreditAccount)
}

// TestHandleVTXOSent verifies that sending VTXOs via OOR is
// recorded as an expense on transfers_out crediting vtxo_balance
// and that SessionID is stored in the dedicated session_id column
// with RoundID left nil.
func TestHandleVTXOSent(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &VTXOSentMsg{
		SessionID: [32]byte{
			0x01,
		},
		AmountSat: 10_000,
	}

	err := run(ctx, a, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountTransfersOut,
		entries[0].DebitAccount)
	require.Equal(t, AccountVTXOBalance,
		entries[0].CreditAccount)
	require.Equal(t, int64(10_000), entries[0].AmountSat)
	require.Equal(t, EventVTXOSent,
		entries[0].EventType)

	// Session identifier lives in session_id, not round_id, so
	// the 32-byte OOR session does not conflict with the
	// 16-byte round idempotency index.
	require.Equal(t, msg.SessionID[:], entries[0].SessionID)
	require.Nil(t, entries[0].RoundID)
}

// TestHandleVTXOSentInRound verifies that a send with RoundID
// set (and SessionID zero) is recorded with round_id populated
// and session_id NULL. Applies to participant-to-participant
// transfers inside a round.
func TestHandleVTXOSentInRound(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &VTXOSentMsg{
		RoundID: [16]byte{
			0xaa,
			0xbb,
			0xcc,
		},
		AmountSat: 25_000,
	}

	err := run(ctx, a, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountTransfersOut,
		entries[0].DebitAccount)
	require.Equal(t, AccountVTXOBalance,
		entries[0].CreditAccount)
	require.Equal(t, msg.RoundID[:], entries[0].RoundID)
	require.Nil(t, entries[0].SessionID)
}

// TestHandleVTXOSentNeitherSet verifies that a send carrying
// neither SessionID nor RoundID is rejected with a clear error.
// Both zero is ambiguous: the actor cannot tell whether the send
// was in-round or out-of-round.
func TestHandleVTXOSentNeitherSet(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &VTXOSentMsg{
		SessionID: [32]byte{},
		RoundID:   [16]byte{},
		AmountSat: 1,
	}

	err := run(ctx, a, msg)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidMessage)
	require.Contains(t, err.Error(),
		"requires one of SessionID or RoundID")
	require.Empty(t, store.getEntries())
}

// TestHandleVTXOSentBothSet verifies that a send carrying both
// SessionID and RoundID is rejected. An in-round send and an
// OOR send are mutually exclusive contexts.
func TestHandleVTXOSentBothSet(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &VTXOSentMsg{
		SessionID: [32]byte{
			0x11,
		},
		RoundID: [16]byte{
			0x22,
		},
		AmountSat: 1,
	}

	err := run(ctx, a, msg)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidMessage)
	require.Contains(
		t, err.Error(),
		"cannot set both SessionID and RoundID",
	)
	require.Empty(t, store.getEntries())
}

// TestHandleVTXOSendReceiveAreGross verifies that a matched
// receive and send of the same amount do not net to zero on a
// single account: transfers_in accumulates credits and
// transfers_out accumulates debits independently.
func TestHandleVTXOSendReceiveAreGross(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	recv := &VTXOReceivedMsg{
		OutpointHash: [32]byte{
			0xaa,
		},
		OutpointIndex: 0,
		AmountSat:     10_000,
		Source:        SourceOOR,
		RoundID: [16]byte{
			1,
		},
	}
	require.NoError(
		t,
		run(
			ctx, a, recv,
		),
	)

	sent := &VTXOSentMsg{
		SessionID: [32]byte{
			0x02,
		},
		AmountSat: 10_000,
	}
	require.NoError(t, run(ctx, a, sent))

	entries := store.getEntries()
	require.Len(t, entries, 2)

	// The receive credits transfers_in; the send debits
	// transfers_out. Neither account nets the other.
	require.Equal(t, AccountTransfersIn, entries[0].CreditAccount)
	require.Equal(t, AccountTransfersOut, entries[1].DebitAccount)
}

// TestHandleExitCost verifies that a unilateral exit books two
// ledger entries that together reduce vtxo_balance by the gross
// AmountSat: a send leg for (AmountSat - ExitCostSat) debiting
// transfers_out and a fee leg for ExitCostSat debiting
// onchain_fees. Both legs credit vtxo_balance.
func TestHandleExitCost(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &ExitCostMsg{
		OutpointHash: [32]byte{
			0xee,
			0xff,
		},
		OutpointIndex: 2,
		AmountSat:     100_000,
		ExitCostSat:   5_000,
		BlockHeight:   800_500,
	}

	err := run(ctx, a, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 2)

	// Send leg: debit transfers_out (net of fee), credit
	// vtxo_balance.
	require.Equal(t, AccountTransfersOut,
		entries[0].DebitAccount)
	require.Equal(t, AccountVTXOBalance,
		entries[0].CreditAccount)
	require.Equal(t, int64(95_000), entries[0].AmountSat)
	require.Equal(t, EventVTXOSent, entries[0].EventType)

	// Fee leg: debit onchain_fees, credit vtxo_balance.
	require.Equal(t, AccountOnchainFees,
		entries[1].DebitAccount)
	require.Equal(t, AccountVTXOBalance,
		entries[1].CreditAccount)
	require.Equal(t, int64(5_000), entries[1].AmountSat)
	require.Equal(t, EventOnchainFeePaid,
		entries[1].EventType)

	// Sanity: the two credit amounts sum to the gross VTXO
	// value, so vtxo_balance drops by the full exited amount.
	require.Equal(
		t, msg.AmountSat, entries[0].AmountSat+entries[1].AmountSat,
	)
}

// TestHandleExitCostOwnWalletDestinationBooksWalletBalance proves an exit
// that paid an output this client's own wallet controls books a third leg
// that cancels the send leg on transfers_out and lands the net value on
// wallet_balance, while the send and fee legs stay byte-for-byte what a
// foreign-destination exit writes.
func TestHandleExitCostOwnWalletDestinationBooksWalletBalance(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &ExitCostMsg{
		OutpointHash: [32]byte{
			0x3c,
		},
		OutpointIndex:        1,
		AmountSat:            100_000,
		ExitCostSat:          5_000,
		BlockHeight:          800_600,
		DestinationOwnWallet: true,
	}

	require.NoError(t, run(ctx, a, msg))

	entries := store.getEntries()
	require.Len(t, entries, 3)

	// Send leg is unchanged by the flag: transfers_out <- vtxo_balance.
	require.Equal(t, AccountTransfersOut, entries[0].DebitAccount)
	require.Equal(t, AccountVTXOBalance, entries[0].CreditAccount)
	require.Equal(t, int64(95_000), entries[0].AmountSat)
	require.Equal(t, EventVTXOSent, entries[0].EventType)

	// Fee leg is unchanged: the miner still took the chain cost.
	require.Equal(t, AccountOnchainFees, entries[1].DebitAccount)
	require.Equal(t, AccountVTXOBalance, entries[1].CreditAccount)
	require.Equal(t, int64(5_000), entries[1].AmountSat)

	// Proceeds leg: wallet_balance <- transfers_out for the net amount,
	// so the outflow nets to zero and the value reappears on-chain.
	require.Equal(t, AccountWalletBalance, entries[2].DebitAccount)
	require.Equal(t, AccountTransfersOut, entries[2].CreditAccount)
	require.Equal(t, int64(95_000), entries[2].AmountSat)
	require.Equal(t, EventVTXOSent, entries[2].EventType)

	// The send and fee keys are the ones a foreign-destination exit
	// writes; the proceeds leg has its own identity.
	sendKey := exitSendIdempotencyKey(
		msg.OutpointHash, msg.OutpointIndex,
	)
	feeKey := exitFeeIdempotencyKey(msg.OutpointHash, msg.OutpointIndex)
	proceedsKey := exitProceedsIdempotencyKey(
		msg.OutpointHash, msg.OutpointIndex,
	)
	require.Equal(t, sendKey, entries[0].IdempotencyKey)
	require.Equal(t, feeKey, entries[1].IdempotencyKey)
	require.Equal(t, proceedsKey, entries[2].IdempotencyKey)
}

// TestHandleExitCostReplayAcrossDestinationFlagDedups proves the reachable
// upgrade replay: an exit booked before the destination flag existed is
// re-emitted by a resumed unroll job with the flag set. The send leg must
// dedup against the row it wrote first rather than book a second credit of
// vtxo_balance, and the proceeds leg must appear exactly once.
func TestHandleExitCostReplayAcrossDestinationFlagDedups(t *testing.T) {
	t.Parallel()

	store := newDedupLedgerStore()
	a := newTestActorWithStore(t, store)
	ctx := t.Context()

	msg := &ExitCostMsg{
		OutpointHash: [32]byte{
			0x4d,
		},
		OutpointIndex: 2,
		AmountSat:     60_000,
		ExitCostSat:   4_000,
		BlockHeight:   800_650,
	}

	// Pre-flag delivery: send leg and fee leg only.
	require.NoError(t, run(ctx, a, msg))
	require.Len(t, store.getEntries(), 2)

	// Post-upgrade re-emission of the same exit with the flag set.
	flagged := *msg
	flagged.DestinationOwnWallet = true
	require.NoError(t, run(ctx, a, &flagged))
	require.NoError(t, run(ctx, a, &flagged))

	entries := store.getEntries()
	require.Len(
		t, entries, 3,
		"replay across the flag must add only the proceeds leg",
	)

	// vtxo_balance is credited once for the net value and once for the
	// fee; wallet_balance is debited once for the net value; and
	// transfers_out nets to zero because the value never left.
	balances := make(map[string]int64)
	for _, entry := range entries {
		balances[entry.DebitAccount] += entry.AmountSat
		balances[entry.CreditAccount] -= entry.AmountSat
	}
	require.Equal(t, int64(-60_000), balances[AccountVTXOBalance])
	require.Equal(t, int64(56_000), balances[AccountWalletBalance])
	require.Equal(t, int64(4_000), balances[AccountOnchainFees])
	require.Zero(t, balances[AccountTransfersOut])
}

// TestExitCostMsgDestinationFlagRoundTrips proves the destination flag
// survives the durable mailbox codec and that a payload written before the
// flag existed decodes to the foreign-destination behaviour it was written
// under, rather than silently re-booking old exits into wallet_balance.
func TestExitCostMsgDestinationFlagRoundTrips(t *testing.T) {
	t.Parallel()

	for _, ownWallet := range []bool{false, true} {
		msg := &ExitCostMsg{
			OutpointHash: [32]byte{
				0x5e,
			},
			OutpointIndex:        3,
			AmountSat:            80_000,
			ExitCostSat:          2_000,
			BlockHeight:          800_700,
			DestinationOwnWallet: ownWallet,
		}

		var buf bytes.Buffer
		require.NoError(t, msg.Encode(&buf))

		var decoded ExitCostMsg
		require.NoError(t, decoded.Decode(bytes.NewReader(buf.Bytes())))
		require.Equal(t, *msg, decoded)
	}

	// A payload written before the flag existed carries no destination
	// record at all, so build the pre-flag stream by hand.
	var (
		outpointHash  = bytes.Repeat([]byte{0x6f}, 32)
		outpointIndex = uint32(0)
		amountSat     = uint64(40_000)
		exitCostSat   = uint64(1_000)
		blockHeight   = uint32(800_800)
	)
	legacyStream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			exitCostOutpointHashType, &outpointHash,
		),
		tlv.MakePrimitiveRecord(
			exitCostOutpointIndexType, &outpointIndex,
		),
		tlv.MakePrimitiveRecord(exitCostAmountSatType, &amountSat),
		tlv.MakePrimitiveRecord(exitCostCostSatType, &exitCostSat),
		tlv.MakePrimitiveRecord(
			exitCostBlockHeightType, &blockHeight,
		),
	)
	require.NoError(t, err)

	var legacyBuf bytes.Buffer
	require.NoError(t, legacyStream.Encode(&legacyBuf))

	var legacy ExitCostMsg
	require.NoError(t, legacy.Decode(bytes.NewReader(legacyBuf.Bytes())))
	require.Equal(t, int64(40_000), legacy.AmountSat)
	require.False(
		t, legacy.DestinationOwnWallet,
		"a missing destination record must keep the old behaviour",
	)
}

// TestHandleExitCostFeeExceedsValue verifies that an exit whose
// fee meets or exceeds the VTXO amount is rejected rather than
// silently producing a non-positive send leg.
func TestHandleExitCostFeeExceedsValue(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &ExitCostMsg{
		OutpointHash: [32]byte{
			0xab,
		},
		OutpointIndex: 0,
		AmountSat:     1_000,
		ExitCostSat:   1_000,
		BlockHeight:   800_600,
	}

	err := run(ctx, a, msg)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidMessage)
	require.Contains(
		t, err.Error(), "exceeds or equals VTXO amount",
	)
	require.Empty(t, store.getEntries())
}

// dedupLedgerStore mirrors the DB-side behavior of the partial
// unique indexes combined with ON CONFLICT DO NOTHING: inserts
// whose (idempotency_key, event_type, debit_account, credit_account)
// already appear are silently dropped. Tests use this to assert
// replay semantics without running a real DB migration.
type dedupLedgerStore struct {
	mu      sync.Mutex
	entries []LedgerEntry
	keys    map[string]struct{}
}

// newDedupLedgerStore constructs a fresh dedupLedgerStore.
func newDedupLedgerStore() *dedupLedgerStore {
	return &dedupLedgerStore{
		keys: make(map[string]struct{}),
	}
}

// InsertLedgerEntry appends the entry unless a previous insert
// already covered the same idempotency_key + account/event tuple,
// in which case the call is a silent no-op. Mirrors the
// idx_client_ledger_idempotent_key partial unique index plus the
// ON CONFLICT DO NOTHING clause on InsertClientLedgerEntry.
func (d *dedupLedgerStore) InsertLedgerEntry(_ context.Context,
	entry LedgerEntry) error {

	d.mu.Lock()
	defer d.mu.Unlock()

	if len(entry.IdempotencyKey) > 0 {
		k := fmt.Sprintf("%x|%s|%s|%s", entry.IdempotencyKey,
			entry.EventType, entry.DebitAccount,
			entry.CreditAccount)
		if _, seen := d.keys[k]; seen {
			return nil
		}
		d.keys[k] = struct{}{}
	}

	d.entries = append(d.entries, entry)

	return nil
}

// getEntries returns a snapshot of the persisted entries.
func (d *dedupLedgerStore) getEntries() []LedgerEntry {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]LedgerEntry{}, d.entries...)
}

// HasSessionEntry scans the persisted entries for a session-keyed match.
func (d *dedupLedgerStore) HasSessionEntry(_ context.Context,
	sessionID [32]byte, eventType, debitAccount, creditAccount string) (
	bool, error) {

	d.mu.Lock()
	defer d.mu.Unlock()

	return hasSessionEntry(
		d.entries, sessionID, eventType, debitAccount, creditAccount,
	)
}

// TestHandleExitCostNamespacesBothLegs verifies that handleExitCost emits the
// send and fee entries under distinct operation-and-leg identities. Both keys
// retain the same outpoint payload, while the namespace prevents either leg
// from colliding with a refresh send for that outpoint.
func TestHandleExitCostNamespacesBothLegs(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &ExitCostMsg{
		OutpointHash: [32]byte{
			0xab,
		},
		OutpointIndex: 7,
		AmountSat:     50_000,
		ExitCostSat:   3_500,
		BlockHeight:   800_800,
	}

	err := run(ctx, a, msg)
	require.NoError(t, err)

	entries := store.getEntries()
	require.Len(t, entries, 2)

	// Send leg: transfers_out <- vtxo_balance for net amount.
	require.Equal(t, AccountTransfersOut, entries[0].DebitAccount)
	require.Equal(
		t, AccountVTXOBalance, entries[0].CreditAccount,
	)
	require.Equal(t, int64(46_500), entries[0].AmountSat)
	require.Equal(t, EventVTXOSent, entries[0].EventType)

	// Fee leg: onchain_fees <- vtxo_balance for the exit cost.
	require.Equal(t, AccountOnchainFees, entries[1].DebitAccount)
	require.Equal(
		t, AccountVTXOBalance, entries[1].CreditAccount,
	)
	require.Equal(t, int64(3_500), entries[1].AmountSat)
	require.Equal(t, EventOnchainFeePaid, entries[1].EventType)

	sendKey := exitSendIdempotencyKey(
		msg.OutpointHash, msg.OutpointIndex,
	)
	feeKey := exitFeeIdempotencyKey(msg.OutpointHash, msg.OutpointIndex)
	require.Equal(t, sendKey, entries[0].IdempotencyKey)
	require.Equal(t, feeKey, entries[1].IdempotencyKey)
	require.NotEqual(t, sendKey, feeKey)
	require.Contains(t, string(sendKey), "ledger:v1:unilateral_exit:send:")
	require.Contains(t, string(feeKey), "ledger:v1:unilateral_exit:fee:")
	for _, entry := range entries {
		require.Equal(t, msg.OutpointHash[:], entry.ChainTxid)
		require.Equal(t, int32(msg.OutpointIndex), *entry.ChainVout)
		require.Equal(
			t, int32(msg.BlockHeight), *entry.ConfirmationHeight,
		)
	}
}

// TestExitOfRefreshOriginVTXOKeepsSendLeg pins the operation namespacing end
// to end: a VTXO minted by a refresh already carries a vtxo_sent row on
// (transfers_out, vtxo_balance) under its own outpoint, and a later
// unilateral exit of that same VTXO must still book its own value row
// through the dedup store rather than be swallowed as a replay.
func TestExitOfRefreshOriginVTXOKeepsSendLeg(t *testing.T) {
	t.Parallel()

	store := newDedupLedgerStore()
	a := newTestActorWithStore(t, store)
	ctx := t.Context()

	var outpoint wire.OutPoint
	outpoint.Hash[0] = 0xcd
	outpoint.Index = 3

	// Refresh pair: the forfeited claim is replaced by a 99,500 sat VTXO
	// at the same outpoint. The pair nets to zero on vtxo_balance.
	require.NoError(
		t,
		run(
			ctx, a, &VTXOSentMsg{
				Outpoint:  outpoint,
				AmountSat: 99_500,
				RoundID:   [16]byte{0x11},
			},
		),
	)
	require.NoError(
		t,
		run(
			ctx, a, &VTXOReceivedMsg{
				OutpointHash:  outpoint.Hash,
				OutpointIndex: outpoint.Index,
				AmountSat:     99_500,
				RoundID:       [16]byte{0x11},
				Source:        SourceRoundRefresh,
			},
		),
	)
	require.Len(t, store.getEntries(), 2)

	// The refreshed VTXO is later exited on-chain with a 2,000 sat sweep
	// fee. Both exit legs must land.
	require.NoError(
		t,
		run(
			ctx, a, &ExitCostMsg{
				OutpointHash:  outpoint.Hash,
				OutpointIndex: outpoint.Index,
				AmountSat:     99_500,
				ExitCostSat:   2_000,
				BlockHeight:   900_000,
			},
		),
	)
	entries := store.getEntries()
	require.Len(t, entries, 4)

	var balance int64
	for _, entry := range entries {
		if entry.DebitAccount == AccountVTXOBalance {
			balance += entry.AmountSat
		}
		if entry.CreditAccount == AccountVTXOBalance {
			balance -= entry.AmountSat
		}
	}
	require.Equal(
		t, int64(-99_500), balance,
		"exit must retire the full VTXO value from vtxo_balance",
	)
}

// TestHandleExitCostReplayIsIdempotent simulates an at-least-once
// redelivery of the same ExitCostMsg and asserts that the store
// still ends up with exactly the two original legs rather than
// four. This validates the combined contract of:
//
//   - two handleExitCost invocations produce four insert calls
//   - each namespaced outpoint-derived IdempotencyKey puts its
//     leg under the partial unique index
//     idx_client_ledger_idempotent_key
//   - ON CONFLICT DO NOTHING at the DB adapter layer turns the
//     second pass into a silent no-op
//
// Dropping any one of those three pieces causes the row count to
// grow and the test to fail.
func TestHandleExitCostReplayIsIdempotent(t *testing.T) {
	t.Parallel()

	store := newDedupLedgerStore()
	a := newTestActorWithStore(t, store)
	ctx := t.Context()

	msg := &ExitCostMsg{
		OutpointHash: [32]byte{
			0xab,
		},
		OutpointIndex: 7,
		AmountSat:     50_000,
		ExitCostSat:   3_500,
		BlockHeight:   800_800,
	}

	// First delivery persists both legs.
	require.NoError(t, run(ctx, a, msg))
	require.Len(t, store.getEntries(), 2)

	// Second delivery of the identical message is the
	// at-least-once replay scenario. Row count must not grow.
	require.NoError(t, run(ctx, a, msg))
	require.Len(
		t, store.getEntries(), 2,
		"replay must not double-book ledger entries",
	)

	// A third run with a different outpoint (different
	// idempotency key) must still persist; this guards against
	// an overzealous dedup that keys only on event_type or
	// only on account pairs.
	other := *msg
	other.OutpointIndex = 8
	require.NoError(
		t,
		run(
			ctx, a, &other,
		),
	)
	require.Len(
		t, store.getEntries(), 4,
		"distinct outpoint must not be deduped",
	)
}

// TestExitFeeIdempotencyKeyDistinguishesOutputs confirms two exited VTXOs that
// share a transaction hash but differ in output index receive distinct fee-leg
// identities.
func TestExitFeeIdempotencyKeyDistinguishesOutputs(t *testing.T) {
	t.Parallel()

	hash := [32]byte{0xde, 0xad}

	k0 := exitFeeIdempotencyKey(hash, 0)
	k1 := exitFeeIdempotencyKey(hash, 1)
	kMax := exitFeeIdempotencyKey(hash, 1<<31)

	require.NotEqual(t, k0, k1)
	require.NotEqual(t, k1, kMax)
	require.NotEqual(t, k0, kMax)
}

// TestLedgerIdempotencyKeysSeparateOperations confirms the exact collision
// domain that refresh and unilateral-exit sends share at the database index is
// split before either row reaches persistence.
func TestLedgerIdempotencyKeysSeparateOperations(t *testing.T) {
	t.Parallel()

	hash := [32]byte{0xde, 0xad}
	refresh := refreshSendIdempotencyKey(hash, 7)
	exitSend := exitSendIdempotencyKey(hash, 7)
	exitFee := exitFeeIdempotencyKey(hash, 7)
	exitProceeds := exitProceedsIdempotencyKey(hash, 7)

	require.NotEqual(t, refresh, exitSend)
	require.NotEqual(t, refresh, exitFee)
	require.NotEqual(t, exitSend, exitFee)
	require.NotEqual(t, exitSend, exitProceeds)
	require.NotEqual(t, exitFee, exitProceeds)
}

// TestHandleExitCostInvalidAmounts verifies non-positive inputs
// are rejected. The table covers the three distinct invalid
// shapes: zero amount (caller forgot the VTXO value), zero fee
// (caller emits before the final sweep cost is known -- this is
// the exact poison-pill the vtxo.emitExitCost no-op guards
// against), and both-zero.
func TestHandleExitCostInvalidAmounts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		amount  int64
		exit    int64
		contain string
	}{
		{
			name:    "zero amount",
			amount:  0,
			exit:    100,
			contain: "positive amount_sat and exit_cost_sat",
		},
		{
			name:    "zero exit cost (poison-pill shape)",
			amount:  10_000,
			exit:    0,
			contain: "positive amount_sat and exit_cost_sat",
		},
		{
			name:    "both zero",
			amount:  0,
			exit:    0,
			contain: "positive amount_sat and exit_cost_sat",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, store := newTestActor(t)
			ctx := t.Context()

			msg := &ExitCostMsg{
				OutpointHash: [32]byte{
					0xcd,
				},
				OutpointIndex: 0,
				AmountSat:     tc.amount,
				ExitCostSat:   tc.exit,
				BlockHeight:   800_700,
			}

			err := run(
				ctx, a, msg,
			)
			require.Error(t, err)
			require.ErrorIs(t, err, ErrInvalidMessage)
			require.Contains(t, err.Error(), tc.contain)
			require.Empty(t, store.getEntries())
		})
	}
}

// TestDBErrorDoesNotWrapErrInvalidMessage verifies that an
// error returned by the underlying ledger store does not wrap
// ErrInvalidMessage, so Receive routes it to WarnS instead of
// ErrorS. This guards against DB transient failures paging.
func TestDBErrorDoesNotWrapErrInvalidMessage(t *testing.T) {
	t.Parallel()

	dbErr := errors.New("simulated db lock contention")
	store := &failingLedgerStore{err: dbErr}

	a := &LedgerActor{
		cfg: ActorConfig{
			LedgerStore: store,
		},
		log: disabledLogger(),
		clk: clock.NewDefaultClock(),
	}

	msg := &FeePaidMsg{
		RoundID: [16]byte{
			1,
		},
		AmountSat:   100,
		FeeType:     FeeTypeBoarding,
		BlockHeight: 1,
	}

	err := run(t.Context(), a, msg)
	require.Error(t, err)
	require.ErrorIs(t, err, dbErr)
	require.NotErrorIs(t, err, ErrInvalidMessage)
}

// failingLedgerStore is a LedgerStore that always returns the
// configured error, used to simulate DB failures in tests.
type failingLedgerStore struct {
	err error
}

func (f *failingLedgerStore) InsertLedgerEntry(_ context.Context,
	_ LedgerEntry) error {

	return f.err
}

func (f *failingLedgerStore) HasSessionEntry(_ context.Context, _ [32]byte, _,
	_, _ string) (bool, error) {

	return false, f.err
}

// TestHandleFeePaidUnknownType verifies that an unknown fee type
// returns an error instead of silently misclassifying the entry.
// TestHandleNonPositiveAmounts exercises the early-return guards
// on every handler that writes a single positive-amount ledger
// entry. A corrupt TLV that decodes to a zero or negative amount
// must surface as ErrInvalidMessage (rejection dead-letters at
// the mailbox layer) rather than hitting the SQL CHECK and
// driving an infinite durable retry.
func TestHandleNonPositiveAmounts(t *testing.T) {
	t.Parallel()

	type handlerFn func(
		ctx context.Context, a *LedgerActor, amt int64,
	) error

	cases := []struct {
		name string
		run  handlerFn
	}{
		{
			name: "FeePaid",
			run: func(ctx context.Context, a *LedgerActor,
				amt int64) error {

				return run(
					ctx, a, &FeePaidMsg{
						RoundID:   [16]byte{1},
						AmountSat: amt,
						FeeType:   FeeTypeBoarding,
					},
				)
			},
		},
		{
			name: "VTXOReceived",
			run: func(ctx context.Context, a *LedgerActor,
				amt int64) error {

				return run(
					ctx, a, &VTXOReceivedMsg{
						OutpointHash: [32]byte{1},
						AmountSat:    amt,
						Source:       SourceOOR,
					},
				)
			},
		},
		{
			name: "VTXOSent",
			run: func(ctx context.Context, a *LedgerActor,
				amt int64) error {

				return run(
					ctx, a, &VTXOSentMsg{
						SessionID: [32]byte{1},
						AmountSat: amt,
					},
				)
			},
		},
	}

	amounts := []int64{0, -1, -1_000}

	for _, tc := range cases {
		for _, amt := range amounts {
			name := fmt.Sprintf("%s amount=%d", tc.name, amt)
			t.Run(name, func(t *testing.T) {
				a, store := newTestActor(t)
				err := tc.run(t.Context(), a, amt)

				require.Error(t, err)
				require.ErrorIs(t, err, ErrInvalidMessage)
				require.Empty(
					t, store.getEntries(),
					"no entry should be written on "+
						"invalid amount",
				)
			})
		}
	}
}

// TestDecodeAmountSatOverflow exercises the int64 narrowing
// guard on the TLV Decode path. A corrupt payload whose satoshi
// field exceeds math.MaxInt64 must surface as ErrInvalidMessage
// rather than silently producing a negative int64 that the
// handler (or the SQL CHECK) would later reject with a less
// actionable error. The single-case structure here keeps the
// addressable temporaries local: tlv.MakePrimitiveRecord needs
// pointers to backing storage, and the test frame happily gives
// them stack lifetimes.
func TestDecodeAmountSatOverflow(t *testing.T) {
	t.Parallel()

	// Full 16-byte RoundID so the fixed-length guard accepts
	// it and the overflow guard is the next thing that fires.
	roundIDArr := [16]byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	}
	roundID := roundIDArr[:]
	over := uint64(math.MaxInt64) + 1
	feeType := []byte("boarding_fee")
	height := uint32(100)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			feePaidRoundIDType, &roundID,
		),
		tlv.MakePrimitiveRecord(
			feePaidAmountSatType, &over,
		),
		tlv.MakePrimitiveRecord(
			feePaidFeeTypeType, &feeType,
		),
		tlv.MakePrimitiveRecord(
			feePaidBlockHeightType, &height,
		),
	)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, stream.Encode(&buf))

	m := &FeePaidMsg{}
	err = m.Decode(&buf)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidMessage)
	require.Contains(t, err.Error(), "exceeds int64 range")
}

func TestHandleFeePaidUnknownType(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &FeePaidMsg{
		RoundID: [16]byte{
			1,
			2,
			3,
		},
		AmountSat:   1500,
		FeeType:     "unknown_type",
		BlockHeight: 800_000,
	}

	err := run(ctx, a, msg)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidMessage)
	require.Contains(t, err.Error(), "unknown fee type")

	// No entry should have been written.
	require.Empty(t, store.getEntries())
}

// TestHandleVTXOReceivedUnknownSource verifies that an unknown
// VTXO source returns an error instead of defaulting to round.
func TestHandleVTXOReceivedUnknownSource(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	msg := &VTXOReceivedMsg{
		OutpointHash: [32]byte{
			0xaa,
			0xbb,
		},
		OutpointIndex: 0,
		AmountSat:     50_000,
		Source:        "collaborative",
		RoundID: [16]byte{
			7,
			8,
			9,
		},
	}

	err := run(ctx, a, msg)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInvalidMessage)
	require.Contains(t, err.Error(), "unknown vtxo source")

	// No entry should have been written.
	require.Empty(t, store.getEntries())
}

// TestHandleUTXOCreated verifies that a UTXO created event is
// recorded in both the wallet_utxo_log audit store AND the
// double-entry ledger. The ledger row books wallet_balance as an
// asset inflow sourced from opening_balance equity so subsequent
// SourceRoundBoarding entries have a non-negative wallet_balance
// to draw from.
func TestHandleUTXOCreated(t *testing.T) {
	t.Parallel()

	a, ledgerStore, auditStore := newTestActorWithAudit(t)
	ctx := t.Context()

	msg := &UTXOCreatedMsg{
		OutpointHash: [32]byte{
			0xaa,
			0xbb,
		},
		OutpointIndex:  0,
		AmountSat:      50_000,
		BlockHeight:    800_000,
		Classification: ClassificationDeposit,
	}

	err := run(ctx, a, msg)
	require.NoError(t, err)

	// Audit-log side: the wallet_utxo_log row is still written.
	audit := auditStore.getEntries()
	require.Len(t, audit, 1)
	require.Equal(t, "created", audit[0].Event)
	require.Equal(t, "deposit", audit[0].ClassifiedAs)
	require.Equal(t, int64(50_000), audit[0].AmountSat)
	require.Equal(t, int32(800_000), audit[0].BlockHeight)

	// Double-entry side: debit wallet_balance, credit
	// opening_balance, stamped with the outpoint-derived
	// idempotency key so a replay is a silent no-op via
	// idx_client_ledger_idempotent_key.
	entries := ledgerStore.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountWalletBalance, entries[0].DebitAccount)
	require.Equal(
		t, AccountOpeningBalance, entries[0].CreditAccount,
	)
	require.Equal(t, int64(50_000), entries[0].AmountSat)
	require.Equal(
		t, EventWalletUTXOCreated, entries[0].EventType,
	)
	require.Equal(
		t, walletUTXOIdempotencyKey(
			msg.OutpointHash, msg.OutpointIndex,
		),
		entries[0].IdempotencyKey, "wallet UTXO ledger entry must "+
			"carry an outpoint-scoped idempotency key for "+
			"replay dedup",
	)
}

// TestHandleUTXOCreatedRejectsNonPositive locks in the validation
// guard: a zero or negative AmountSat on UTXOCreatedMsg is a
// malformed caller payload (impossible on-chain but reachable
// via a corrupt TLV). The handler must return ErrInvalidMessage
// and write nothing to either store, so a malformed durable
// message dead-letters cleanly instead of hitting the SQL
// CHECK (amount_sat > 0) and driving an infinite retry.
func TestHandleUTXOCreatedRejectsNonPositive(t *testing.T) {
	t.Parallel()

	a, ledgerStore, auditStore := newTestActorWithAudit(t)
	ctx := t.Context()

	for _, amt := range []int64{0, -1, -50_000} {
		err := run(
			ctx, a, &UTXOCreatedMsg{
				OutpointHash:   [32]byte{0xde},
				OutpointIndex:  0,
				AmountSat:      amt,
				BlockHeight:    800_000,
				Classification: ClassificationDeposit,
			},
		)
		require.ErrorIs(t, err, ErrInvalidMessage)
	}

	require.Empty(t, ledgerStore.getEntries())
	require.Empty(t, auditStore.getEntries())
}

// TestHandleUTXOSpent verifies that a UTXO spent event is
// recorded in the audit store with the correct fields.
func TestHandleUTXOSpent(t *testing.T) {
	t.Parallel()

	a, _, auditStore := newTestActorWithAudit(t)
	ctx := t.Context()

	msg := &UTXOSpentMsg{
		OutpointHash: [32]byte{
			0xcc,
			0xdd,
		},
		OutpointIndex:  1,
		AmountSat:      25_000,
		BlockHeight:    800_050,
		Classification: ClassificationRoundFunding,
	}

	err := run(ctx, a, msg)
	require.NoError(t, err)

	entries := auditStore.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, "spent", entries[0].Event)
	require.Equal(t, "round_funding", entries[0].ClassifiedAs)
	require.Equal(t, int64(25_000), entries[0].AmountSat)
	require.Equal(t, int32(800_050), entries[0].BlockHeight)
}

// TestHandleUTXOSpentNonPositiveGuardScoped verifies that the
// non-positive amount guard only fires for the boarding-sweep-input
// classification, which is the sole branch that books a ledger leg
// (debit wallet_clearing) subject to the SQL CHECK (amount_sat > 0).
// Audit-only classifications must tolerate a zero amount (the
// wallet_utxo_log table has no positivity constraint), so they record
// the audit row instead of dead-lettering on a poison-pill TLV.
func TestHandleUTXOSpentNonPositiveGuardScoped(t *testing.T) {
	t.Parallel()

	t.Run("audit-only tolerates zero amount", func(t *testing.T) {
		t.Parallel()

		a, ledgerStore, auditStore := newTestActorWithAudit(t)

		err := run(t.Context(), a, &UTXOSpentMsg{
			OutpointHash:   [32]byte{0xab},
			OutpointIndex:  0,
			AmountSat:      0,
			BlockHeight:    800_000,
			Classification: ClassificationRoundFunding,
		})
		require.NoError(t, err)

		require.Len(t, auditStore.getEntries(), 1)
		require.Empty(
			t, ledgerStore.getEntries(),
			"audit-only spend must not book a ledger leg",
		)
	})

	t.Run("boarding sweep input rejects zero amount", func(t *testing.T) {
		t.Parallel()

		a, ledgerStore, auditStore := newTestActorWithAudit(t)

		err := run(t.Context(), a, &UTXOSpentMsg{
			OutpointHash:   [32]byte{0xcd},
			OutpointIndex:  0,
			AmountSat:      0,
			BlockHeight:    800_000,
			Classification: ClassificationBoardingSweepInput,
		})
		require.Error(t, err)
		require.ErrorIs(t, err, ErrInvalidMessage)

		require.Empty(
			t, auditStore.getEntries(),
			"rejected spend must not write an audit row",
		)
		require.Empty(t, ledgerStore.getEntries())
	})
}

// boardingSweepInput builds a SweepInput from a one-byte hash seed.
func boardingSweepInput(hashByte byte, index uint32, amt int64) SweepInput {
	return SweepInput{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				hashByte,
			},
			Index: index,
		},
		AmountSat: amt,
	}
}

// TestHandleBoardingSweepConfirmedNetsToZero verifies the consolidated
// boarding-sweep handler books the fee, per-input, and destination legs in
// one commit and that wallet_clearing nets to zero for both the
// wallet-return and external-destination paths. It also confirms the audit
// rows land alongside the balance legs.
func TestHandleBoardingSweepConfirmedNetsToZero(t *testing.T) {
	t.Parallel()

	const (
		in1       = int64(40_000)
		in2       = int64(60_000)
		total     = in1 + in2
		fee       = int64(444)
		anchor    = int64(330)
		chainCost = fee + anchor
		dest      = total - chainCost
	)

	cases := []struct {
		name         string
		external     bool
		wantAudit    int
		wantTransfer bool
	}{
		{
			name:      "wallet return",
			external:  false,
			wantAudit: 3, // two spent inputs + one created return
		},
		{
			name:         "external destination",
			external:     true,
			wantAudit:    2, // two spent inputs only
			wantTransfer: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a, ledgerStore, auditStore := newTestActorWithAudit(t)

			msg := &BoardingSweepConfirmedMsg{
				Txid: [32]byte{
					0x7a,
				},
				BlockHeight:  800_800,
				ChainCostSat: chainCost,
				Inputs: []SweepInput{
					boardingSweepInput(0xa1, 0, in1),
					boardingSweepInput(0xb2, 1, in2),
				},
				DestinationSat:      dest,
				DestinationExternal: tc.external,
			}

			require.NoError(t, run(t.Context(), a, msg))

			balances := map[string]int64{}
			for _, e := range ledgerStore.getEntries() {
				balances[e.DebitAccount] += e.AmountSat
				balances[e.CreditAccount] -= e.AmountSat
			}

			require.Equal(
				t, int64(0), balances[AccountWalletClearing],
				"wallet_clearing must net to zero",
			)
			require.Equal(
				t, chainCost, balances[AccountOnchainFees],
				"onchain_fees debited by chain cost",
			)

			require.Len(t, auditStore.getEntries(), tc.wantAudit)

			if tc.wantTransfer {
				require.Equal(
					t, dest, balances[AccountTransfersOut],
					"external dest debits transfers_out",
				)
			}
		})
	}
}

// TestHandleBoardingSweepConfirmedRejectsInvalid locks in the up-front
// validation guards so a malformed message dead-letters cleanly rather than
// writing a partial leg set or hitting a SQL CHECK mid-commit.
func TestHandleBoardingSweepConfirmedRejectsInvalid(t *testing.T) {
	t.Parallel()

	base := func() *BoardingSweepConfirmedMsg {
		return &BoardingSweepConfirmedMsg{
			Txid: [32]byte{
				0x9c,
			},
			BlockHeight:  800_000,
			ChainCostSat: 774,
			Inputs: []SweepInput{
				boardingSweepInput(0xa1, 0, 100_000),
			},
			DestinationSat:      99_226,
			DestinationExternal: false,
		}
	}

	cases := []struct {
		name   string
		mutate func(*BoardingSweepConfirmedMsg)
	}{
		{
			name: "non-positive chain cost",
			mutate: func(m *BoardingSweepConfirmedMsg) {
				m.ChainCostSat = 0
			},
		},
		{
			name: "non-positive destination",
			mutate: func(m *BoardingSweepConfirmedMsg) {
				m.DestinationSat = -1
			},
		},
		{
			name: "no inputs",
			mutate: func(m *BoardingSweepConfirmedMsg) {
				m.Inputs = nil
			},
		},
		{
			name: "non-positive input amount",
			mutate: func(m *BoardingSweepConfirmedMsg) {
				m.Inputs = []SweepInput{
					boardingSweepInput(0xa1, 0, 0),
				}
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a, ledgerStore, auditStore := newTestActorWithAudit(t)

			msg := base()
			tc.mutate(msg)

			err := run(t.Context(), a, msg)
			require.Error(t, err)
			require.ErrorIs(t, err, ErrInvalidMessage)

			require.Empty(t, ledgerStore.getEntries())
			require.Empty(t, auditStore.getEntries())
		})
	}
}

// TestBoardingRoundNetsToOpeningBalanceAndVTXO is a scenario-level
// test that walks the full boarding flow: a wallet UTXO confirms
// (booked as a deposit via handleUTXOCreated) and is then consumed
// by a round (booked via handleVTXOReceived with
// SourceRoundBoarding). It reconstructs the per-account running
// balance from the recorded legs and asserts the four invariants
// a boarding round must satisfy:
//   - wallet_balance nets to zero (deposit credit cancels round
//     debit).
//   - vtxo_balance rises by exactly the boarded amount.
//   - opening_balance rises by exactly the boarded amount
//     (representing the equity source of the funds).
//   - no other account is touched.
//
// The test leaves FeePaidMsg out so it can isolate the core
// deposit-boarding pairing. Fee rows are covered by dedicated fee
// handler tests.
func TestBoardingRoundNetsToOpeningBalanceAndVTXO(t *testing.T) {
	t.Parallel()

	a, ledgerStore, _ := newTestActorWithAudit(t)
	ctx := t.Context()

	const amount int64 = 80_000
	outpoint := [32]byte{0x11}
	roundID := [16]byte{0xaa, 0xbb}

	// Leg 1: wallet UTXO confirms.
	require.NoError(
		t,
		run(
			ctx, a, &UTXOCreatedMsg{
				OutpointHash:   outpoint,
				OutpointIndex:  3,
				AmountSat:      amount,
				BlockHeight:    800_000,
				Classification: ClassificationDeposit,
			},
		),
	)

	// Leg 2: same UTXO is spent into a round, producing an owned
	// VTXO with Source=SourceRoundBoarding.
	require.NoError(
		t,
		run(
			ctx, a, &VTXOReceivedMsg{
				OutpointHash:  [32]byte{0x22},
				OutpointIndex: 0,
				AmountSat:     amount,
				Source:        SourceRoundBoarding,
				RoundID:       roundID,
			},
		),
	)

	balances := map[string]int64{}
	for _, e := range ledgerStore.getEntries() {
		balances[e.DebitAccount] += e.AmountSat
		balances[e.CreditAccount] -= e.AmountSat
	}

	require.Equal(
		t, int64(0), balances[AccountWalletBalance],
		"deposit + boarding must cancel on wallet_balance",
	)
	require.Equal(
		t, amount, balances[AccountVTXOBalance],
		"vtxo_balance must rise by the boarded amount",
	)
	require.Equal(
		t, -amount, balances[AccountOpeningBalance], "opening_balanc"+
			"e credits rise by the boarded amount (negative "+
			"balance reflects equity-normal side)",
	)
	require.Equal(
		t, int64(0), balances[AccountTransfersIn],
		"boarding must not touch transfers_in",
	)
	require.Equal(
		t, int64(0), balances[AccountTransfersOut],
		"boarding must not touch transfers_out",
	)
	require.Equal(
		t, int64(0), balances[AccountFeesPaid],
		"isolated boarding pair should not include a fee leg",
	)
}

// TestHandleUTXOCreatedNoAuditStore verifies that UTXO created
// handling succeeds gracefully when no audit store is configured.
func TestHandleUTXOCreatedNoAuditStore(t *testing.T) {
	t.Parallel()

	a, _ := newTestActor(t)
	ctx := t.Context()

	msg := &UTXOCreatedMsg{
		OutpointHash: [32]byte{
			0xaa,
		},
		OutpointIndex:  0,
		AmountSat:      10_000,
		BlockHeight:    800_000,
		Classification: ClassificationDeposit,
	}

	// Should not error even without UTXOAuditStore.
	err := run(ctx, a, msg)
	require.NoError(t, err)
}

// TestMessageTLVRoundTrip verifies that all client message types
// can be encoded and decoded without data loss.
func TestMessageTLVRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  LedgerMsg
		new  func() LedgerMsg
	}{
		{
			name: "FeePaid",
			msg: &FeePaidMsg{
				RoundID: [16]byte{
					1,
					2,
					3,
				},
				AmountSat:   999,
				FeeType:     FeeTypeBoarding,
				BlockHeight: 800_000,
			},
			new: func() LedgerMsg {
				return &FeePaidMsg{}
			},
		},
		{
			name: "VTXOReceived",
			msg: &VTXOReceivedMsg{
				OutpointHash: [32]byte{
					0xaa,
				},
				OutpointIndex: 42,
				AmountSat:     50_000,
				Source:        SourceOOR,
				RoundID: [16]byte{
					4,
					5,
					6,
				},
			},
			new: func() LedgerMsg {
				return &VTXOReceivedMsg{}
			},
		},
		{
			name: "VTXOSentOOR",
			msg: &VTXOSentMsg{
				SessionID: [32]byte{
					0xbb,
				},
				AmountSat: 10_000,
			},
			new: func() LedgerMsg {
				return &VTXOSentMsg{}
			},
		},
		{
			name: "VTXOSentInRound",
			msg: &VTXOSentMsg{
				RoundID: [16]byte{
					0xcc,
					0xdd,
				},
				AmountSat: 20_000,
			},
			new: func() LedgerMsg {
				return &VTXOSentMsg{}
			},
		},
		{
			name: "ExitCost",
			msg: &ExitCostMsg{
				OutpointHash: [32]byte{
					0xcc,
				},
				OutpointIndex: 1,
				AmountSat:     100_000,
				ExitCostSat:   5_000,
				BlockHeight:   800_500,
			},
			new: func() LedgerMsg {
				return &ExitCostMsg{}
			},
		},
		{
			name: "UTXOCreated",
			msg: &UTXOCreatedMsg{
				OutpointHash: [32]byte{
					0xdd,
				},
				OutpointIndex:  7,
				AmountSat:      30_000,
				BlockHeight:    800_200,
				Classification: ClassificationDeposit,
			},
			new: func() LedgerMsg {
				return &UTXOCreatedMsg{}
			},
		},
		{
			name: "UTXOSpent",
			msg: &UTXOSpentMsg{
				OutpointHash: [32]byte{
					0xee,
				},
				OutpointIndex:  2,
				AmountSat:      45_000,
				BlockHeight:    800_300,
				Classification: ClassificationRoundFunding,
			},
			new: func() LedgerMsg {
				return &UTXOSpentMsg{}
			},
		},
		{
			name: "BoardingSweepConfirmedExternal",
			msg: &BoardingSweepConfirmedMsg{
				Txid: [32]byte{
					0xa1,
				},
				BlockHeight:  800_400,
				ChainCostSat: 774,
				Inputs: []SweepInput{
					{
						Outpoint: wire.OutPoint{
							Hash: chainhash.Hash{
								0xb2,
							},
							Index: 0,
						},
						AmountSat: 40_000,
					},
					{
						Outpoint: wire.OutPoint{
							Hash: chainhash.Hash{
								0xc3,
							},
							Index: 3,
						},
						AmountSat: 60_000,
					},
				},
				DestinationSat:      99_226,
				DestinationExternal: true,
			},
			new: func() LedgerMsg {
				return &BoardingSweepConfirmedMsg{}
			},
		},
		{
			name: "BoardingSweepConfirmedReturn",
			msg: &BoardingSweepConfirmedMsg{
				Txid: [32]byte{
					0xd4,
				},
				BlockHeight:  800_410,
				ChainCostSat: 500,
				Inputs: []SweepInput{
					{
						Outpoint: wire.OutPoint{
							Hash: chainhash.Hash{
								0xe5,
							},
							Index: 1,
						},
						AmountSat: 25_000,
					},
				},
				DestinationSat:      24_500,
				DestinationExternal: false,
			},
			new: func() LedgerMsg {
				return &BoardingSweepConfirmedMsg{}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Encode.
			var buf []byte
			w := &bytesWriter{buf: &buf}
			err := tc.msg.Encode(w)
			require.NoError(t, err)

			// Decode.
			decoded := tc.new()
			r := &bytesReader{buf: buf}
			err = decoded.Decode(r)
			require.NoError(t, err)

			// Verify TLV type and field content
			// match after round-trip.
			require.Equal(t,
				tc.msg.TLVType(),
				decoded.TLVType(),
			)
			require.Equal(t, tc.msg, decoded)
		})
	}
}

// bytesWriter is a simple io.Writer backed by a byte slice.
type bytesWriter struct {
	buf *[]byte
}

func (w *bytesWriter) Write(p []byte) (int, error) {
	*w.buf = append(*w.buf, p...)

	return len(p), nil
}

// bytesReader is a simple io.Reader backed by a byte slice.
type bytesReader struct {
	buf []byte
	off int
}

func (r *bytesReader) Read(p []byte) (int, error) {
	if r.off >= len(r.buf) {
		return 0, io.EOF
	}

	n := copy(p, r.buf[r.off:])
	r.off += n

	return n, nil
}

// TestHandleVTXOSentOwnWalletProceedsBooksWalletBalance proves a cooperative
// leave that paid a script the daemon's own backing wallet minted books a
// second leg cancelling the send leg on transfers_out, while the send leg's
// accounts and identity stay exactly what a foreign-destination leave writes.
func TestHandleVTXOSentOwnWalletProceedsBooksWalletBalance(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	sendKey := []byte("round-outflow:round-a:leave:0")
	msg := &VTXOSentMsg{
		RoundID: [16]byte{
			0x7a,
		},
		AmountSat:         50_000,
		IdempotencyKey:    sendKey,
		ProceedsOwnWallet: true,
	}

	require.NoError(t, run(ctx, a, msg))

	entries := store.getEntries()
	require.Len(t, entries, 2)

	// Send leg: unchanged by the flag, because its accounts are part of
	// the dedup tuple.
	require.Equal(t, AccountTransfersOut, entries[0].DebitAccount)
	require.Equal(t, AccountVTXOBalance, entries[0].CreditAccount)
	require.Equal(t, int64(50_000), entries[0].AmountSat)
	require.Equal(t, sendKey, entries[0].IdempotencyKey)

	// Proceeds leg: wallet_balance <- transfers_out, separately keyed.
	require.Equal(t, AccountWalletBalance, entries[1].DebitAccount)
	require.Equal(t, AccountTransfersOut, entries[1].CreditAccount)
	require.Equal(t, int64(50_000), entries[1].AmountSat)
	require.Equal(t, EventVTXOSent, entries[1].EventType)
	require.Equal(
		t, sendProceedsIdempotencyKey(sendKey),
		entries[1].IdempotencyKey,
	)
	require.NotEqual(
		t, entries[0].IdempotencyKey, entries[1].IdempotencyKey,
	)

	// The leave nets to an internal transfer: vtxo_balance down,
	// wallet_balance up, transfers_out flat.
	balances := make(map[string]int64)
	for _, entry := range entries {
		balances[entry.DebitAccount] += entry.AmountSat
		balances[entry.CreditAccount] -= entry.AmountSat
	}
	require.Equal(t, int64(-50_000), balances[AccountVTXOBalance])
	require.Equal(t, int64(50_000), balances[AccountWalletBalance])
	require.Zero(t, balances[AccountTransfersOut])
}

// TestHandleVTXOSentForeignDestinationWritesNoProceedsLeg proves the default
// stays a real outflow: without the flag the send leg stands alone.
func TestHandleVTXOSentForeignDestinationWritesNoProceedsLeg(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	require.NoError(
		t,
		run(
			ctx, a, &VTXOSentMsg{
				RoundID:   [16]byte{0x7b},
				AmountSat: 50_000,
				IdempotencyKey: []byte(
					"round-outflow:round-b:leave:0",
				),
			},
		),
	)

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountTransfersOut, entries[0].DebitAccount)
}

// TestHandleVTXOSentProceedsReplayDedups proves the reachable upgrade replay:
// a leave booked before the flag existed, re-emitted with the flag set, must
// add only the proceeds leg rather than credit vtxo_balance a second time.
func TestHandleVTXOSentProceedsReplayDedups(t *testing.T) {
	t.Parallel()

	store := newDedupLedgerStore()
	a := newTestActorWithStore(t, store)
	ctx := t.Context()

	msg := &VTXOSentMsg{
		RoundID: [16]byte{
			0x7c,
		},
		AmountSat:      30_000,
		IdempotencyKey: []byte("round-outflow:round-c:leave:0"),
	}

	require.NoError(t, run(ctx, a, msg))
	require.Len(t, store.getEntries(), 1)

	flagged := *msg
	flagged.ProceedsOwnWallet = true
	require.NoError(t, run(ctx, a, &flagged))
	require.NoError(t, run(ctx, a, &flagged))

	entries := store.getEntries()
	require.Len(
		t, entries, 2,
		"replay across the flag must add only the proceeds leg",
	)

	balances := make(map[string]int64)
	for _, entry := range entries {
		balances[entry.DebitAccount] += entry.AmountSat
		balances[entry.CreditAccount] -= entry.AmountSat
	}
	require.Equal(t, int64(-30_000), balances[AccountVTXOBalance])
	require.Equal(t, int64(30_000), balances[AccountWalletBalance])
	require.Zero(t, balances[AccountTransfersOut])
}

// TestHandleVTXOSentProceedsNeedsAKey proves a flagged send with no
// idempotency key is rejected rather than quietly booking only the send leg.
// The proceeds leg is keyed off the send's key, and the paired leave_proceeds
// audit row suppresses the deposit credit on the strength of that leg
// existing, so dropping it would strand the value uncredited in both places.
func TestHandleVTXOSentProceedsNeedsAKey(t *testing.T) {
	t.Parallel()

	a, store := newTestActor(t)
	ctx := t.Context()

	err := run(
		ctx, a, &VTXOSentMsg{
			SessionID:         [32]byte{0x7d},
			AmountSat:         10_000,
			ProceedsOwnWallet: true,
		},
	)
	require.ErrorIs(t, err, ErrInvalidMessage)
	require.Empty(t, store.getEntries())
}

// TestVTXOSentMsgProceedsFlagRoundTrips proves the flag survives the durable
// mailbox codec and that a payload written before it existed decodes to the
// foreign-destination booking it was written under.
func TestVTXOSentMsgProceedsFlagRoundTrips(t *testing.T) {
	t.Parallel()

	for _, ownWallet := range []bool{false, true} {
		msg := &VTXOSentMsg{
			RoundID: [16]byte{
				0x8a,
			},
			AmountSat:         70_000,
			IdempotencyKey:    []byte("leave-key"),
			ProceedsOwnWallet: ownWallet,
		}

		var buf bytes.Buffer
		require.NoError(t, msg.Encode(&buf))

		var decoded VTXOSentMsg
		require.NoError(t, decoded.Decode(bytes.NewReader(buf.Bytes())))
		require.Equal(t, *msg, decoded)
	}

	// A payload written before the flag existed carries no proceeds
	// record at all, so build the pre-flag stream by hand.
	var (
		sessionID      = make([]byte, 32)
		amountSat      = uint64(20_000)
		roundID        = make([]byte, 16)
		outpoint       = outpointRecord{}
		idempotencyKey = []byte("legacy-leave-key")
	)
	roundID[0] = 0x8b

	legacyStream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(vtxoSentSessionIDType, &sessionID),
		tlv.MakePrimitiveRecord(vtxoSentAmountSatType, &amountSat),
		tlv.MakePrimitiveRecord(vtxoSentRoundIDType, &roundID),
		makeOutpointRecord(vtxoSentOutpointType, &outpoint),
		tlv.MakePrimitiveRecord(
			vtxoSentIdempotencyType, &idempotencyKey,
		),
	)
	require.NoError(t, err)

	var legacyBuf bytes.Buffer
	require.NoError(t, legacyStream.Encode(&legacyBuf))

	var legacy VTXOSentMsg
	require.NoError(t, legacy.Decode(bytes.NewReader(legacyBuf.Bytes())))
	require.Equal(t, int64(20_000), legacy.AmountSat)
	require.False(
		t, legacy.ProceedsOwnWallet,
		"a missing proceeds record must keep the old behaviour",
	)
}

// TestHandleUTXOCreatedExitProceedsIsAuditOnly proves the sweep output a
// unilateral exit paid to the wallet is recorded in the audit log but books
// no ledger leg. The ExitCostMsg proceeds leg already credited wallet_balance
// for exactly this value; a deposit leg here would credit it twice.
func TestHandleUTXOCreatedExitProceedsIsAuditOnly(t *testing.T) {
	t.Parallel()

	a, ledgerStore, auditStore := newTestActorWithAudit(t)
	ctx := t.Context()

	require.NoError(
		t,
		run(
			ctx, a, &UTXOCreatedMsg{
				OutpointHash:   [32]byte{0x91},
				OutpointIndex:  0,
				AmountSat:      95_000,
				BlockHeight:    900_000,
				Classification: ClassificationExitProceeds,
			},
		),
	)

	require.Empty(
		t, ledgerStore.getEntries(),
		"exit proceeds must not book a second wallet_balance credit",
	)

	audits := auditStore.getEntries()
	require.Len(t, audits, 1)
	require.Equal(t, "created", audits[0].Event)
	require.Equal(t, ClassificationExitProceeds, audits[0].ClassifiedAs)
	require.Equal(t, int64(95_000), audits[0].AmountSat)
}

// proceedsOutpoint is a convenient outpoint built from a one-byte hash seed.
func proceedsOutpoint(seed byte, index uint32) wire.OutPoint {
	var hash chainhash.Hash
	hash[0] = seed

	return wire.OutPoint{Hash: hash, Index: index}
}

// depositMsg builds a boarding-deposit UTXOCreatedMsg funded by the given
// previous outpoints.
func depositMsg(seed byte, amount int64,
	inputs ...wire.OutPoint) *UTXOCreatedMsg {

	return &UTXOCreatedMsg{
		OutpointHash: [32]byte{
			seed,
		},
		OutpointIndex:  0,
		AmountSat:      amount,
		BlockHeight:    900_100,
		Classification: ClassificationDeposit,
		FundingInputs:  inputs,
	}
}

// proceedsMsg builds an own-wallet proceeds UTXOCreatedMsg at an outpoint.
func proceedsMsg(outpoint wire.OutPoint, amount int64,
	classification string) *UTXOCreatedMsg {

	return &UTXOCreatedMsg{
		OutpointHash:   [32]byte(outpoint.Hash),
		OutpointIndex:  outpoint.Index,
		AmountSat:      amount,
		BlockHeight:    900_000,
		Classification: classification,
	}
}

// netBalances folds a ledger entry list into a per-account signed total.
func netBalances(entries []LedgerEntry) map[string]int64 {
	balances := make(map[string]int64)
	for _, entry := range entries {
		balances[entry.DebitAccount] += entry.AmountSat
		balances[entry.CreditAccount] -= entry.AmountSat
	}

	return balances
}

// TestRecycledProceedsReverseInEitherOrder is the core property of the
// recycled-proceeds design: the reversing leg is booked by whichever of the
// two messages commits second, so the outcome does not depend on the order
// two independent producers happen to reach the ledger in.
//
// The same table covers all three proceeds classifications, because the whole
// point of the family is that the reversal does not care which one produced
// the credit.
func TestRecycledProceedsReverseInEitherOrder(t *testing.T) {
	t.Parallel()

	classifications := []string{
		ClassificationExitProceeds,
		ClassificationLeaveProceeds,
		ClassificationRecycledChange,
	}

	orders := []struct {
		name         string
		proceedsLast bool
	}{
		{
			name:         "proceeds first",
			proceedsLast: false,
		},
		{
			name:         "deposit first",
			proceedsLast: true,
		},
	}

	for _, classification := range classifications {
		for _, order := range orders {
			name := classification + "/" + order.name
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				assertReversal(
					t, classification, order.proceedsLast,
				)
			})
		}
	}
}

// assertReversal runs one proceeds/deposit pair in the requested order and
// checks that wallet_balance ends up crediting the coins exactly once.
func assertReversal(t *testing.T, classification string, proceedsLast bool) {
	t.Helper()

	store := newDedupLedgerStore()
	a := newTestActorWithStore(t, store)
	audit := &mockUTXOAuditStore{}
	a.cfg.UTXOAuditStore = audit
	ctx := t.Context()

	const amount = 90_000
	input := proceedsOutpoint(0x92, 0)
	proceeds := proceedsMsg(input, amount, classification)
	deposit := depositMsg(0x93, amount, input)

	msgs := []*UTXOCreatedMsg{proceeds, deposit}
	if proceedsLast {
		msgs = []*UTXOCreatedMsg{deposit, proceeds}
	}
	for _, msg := range msgs {
		require.NoError(t, run(ctx, a, msg))
	}

	entries := store.getEntries()
	balances := netBalances(entries)

	// Exactly one credit of these coins survives, whichever order the two
	// messages arrived in.
	//
	// For exit and leave proceeds the credit was booked by an earlier
	// message outside this handler, so the deposit's credit and the
	// reversal cancel and the net here is zero. Recycled change books its
	// own credit in this handler, so its net is one amount. Either way
	// the coins are counted once and never twice.
	want := int64(0)
	if classification == ClassificationRecycledChange {
		want = amount
	}
	require.Equal(
		t, want, balances[AccountWalletBalance],
		"recycled proceeds must be credited exactly once",
	)

	var reversals int
	for _, entry := range entries {
		if entry.DebitAccount != AccountOpeningBalance ||
			entry.CreditAccount != AccountWalletBalance {

			continue
		}

		reversals++
		require.Equal(t, int64(amount), entry.AmountSat)
		require.Equal(t, EventWalletUTXOSpent, entry.EventType)

		// The reversing leg's key is namespaced by classification, so
		// it can never collide with the boarding-sweep input leg that
		// keys on the bare outpoint and books different accounts.
		hash := [32]byte(input.Hash)
		require.NotEqual(
			t, walletUTXOIdempotencyKey(hash, input.Index),
			entry.IdempotencyKey,
		)
		require.Equal(
			t, classifiedUTXOIdempotencyKey(
				ClassificationDepositFunding, hash, input.Index,
			),
			entry.IdempotencyKey,
		)
	}
	require.Equal(t, 1, reversals)

	// The spend is recorded in the audit log under the classification the
	// reversal booked, with the real amount.
	var spends int
	for _, entry := range audit.getEntries() {
		if entry.Event != "spent" {
			continue
		}

		spends++
		require.Equal(
			t, ClassificationDepositFunding, entry.ClassifiedAs,
		)
		require.Equal(t, int64(amount), entry.AmountSat)
	}
	require.Equal(t, 1, spends)
}

// TestRecycledProceedsReplayDedups proves the reversing leg survives
// at-least-once delivery: replaying either message books no second reversal,
// and a boarding-sweep input spend naming the same outpoint still books its
// own distinct leg.
func TestRecycledProceedsReplayDedups(t *testing.T) {
	t.Parallel()

	store := newDedupLedgerStore()
	a := newTestActorWithStore(t, store)
	a.cfg.UTXOAuditStore = &mockUTXOAuditStore{}
	ctx := t.Context()

	input := proceedsOutpoint(0x94, 1)
	proceeds := proceedsMsg(input, 40_000, ClassificationExitProceeds)
	deposit := depositMsg(0x95, 40_000, input)

	for _, msg := range []*UTXOCreatedMsg{
		proceeds, deposit, proceeds, deposit,
	} {
		require.NoError(t, run(ctx, a, msg))
	}

	before := store.getEntries()
	require.Equal(
		t, int64(0), netBalances(before)[AccountWalletBalance],
		"replay must not re-credit or re-reverse",
	)

	sweepClass := ClassificationBoardingSweepInput
	sweepInput := &UTXOSpentMsg{
		OutpointHash:   [32]byte(input.Hash),
		OutpointIndex:  input.Index,
		AmountSat:      40_000,
		BlockHeight:    900_200,
		Classification: sweepClass,
	}
	require.NoError(t, run(ctx, a, sweepInput))

	require.Len(
		t, store.getEntries(), len(before)+1,
		"the two classifications book distinct legs for one outpoint",
	)
}

// TestPartialSpendChangeIsCreditedBack proves the arithmetic of a partial
// spend. Boarding part of a proceeds UTXO reverses the whole input, so the
// change that came straight back must be credited again or the client is
// understated by it -- and boarding that change a generation later must
// reverse it in turn rather than counting it twice.
func TestPartialSpendChangeIsCreditedBack(t *testing.T) {
	t.Parallel()

	store := newDedupLedgerStore()
	a := newTestActorWithStore(t, store)
	a.cfg.UTXOAuditStore = &mockUTXOAuditStore{}
	ctx := t.Context()

	const (
		proceedsSat = 100_000
		boardedSat  = 60_000
		changeSat   = 39_000
	)

	// Generation one: exit proceeds, partly boarded.
	exit := proceedsOutpoint(0x96, 0)
	require.NoError(
		t,
		run(
			ctx, a, proceedsMsg(
				exit, proceedsSat, ClassificationExitProceeds,
			),
		),
	)
	require.NoError(t, run(ctx, a, depositMsg(0x97, boardedSat, exit)))

	change := proceedsOutpoint(0x97, 1)
	require.NoError(
		t,
		run(
			ctx, a, proceedsMsg(
				change, changeSat, ClassificationRecycledChange,
			),
		),
	)

	// The exit's proceeds leg credited wallet_balance outside this
	// handler, so here the net is the boarded deposit plus the change,
	// minus the reversal of the whole input.
	balances := netBalances(store.getEntries())
	require.Equal(
		t, int64(boardedSat+changeSat-proceedsSat),
		balances[AccountWalletBalance],
	)

	// Generation two: the change is boarded in turn.
	require.NoError(t, run(ctx, a, depositMsg(0x98, changeSat, change)))

	balances = netBalances(store.getEntries())
	require.Equal(
		t, int64(boardedSat+changeSat-proceedsSat),
		balances[AccountWalletBalance],
		"boarding the change must reverse its credit, not add one",
	)
}

// TestDepositIgnoresForeignFundingInputs proves a deposit funded by coins the
// client never credited books nothing extra: no reversal, and no audit row
// that would make a stranger's outpoint look like one of ours.
func TestDepositIgnoresForeignFundingInputs(t *testing.T) {
	t.Parallel()

	store := newDedupLedgerStore()
	a := newTestActorWithStore(t, store)
	audit := &mockUTXOAuditStore{}
	a.cfg.UTXOAuditStore = audit
	ctx := t.Context()

	foreign := proceedsOutpoint(0x99, 3)
	require.NoError(t, run(ctx, a, depositMsg(0x9a, 70_000, foreign)))

	entries := store.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountWalletBalance, entries[0].DebitAccount)

	for _, entry := range audit.getEntries() {
		require.Equal(
			t, "created", entry.Event, "a foreign funding "+
				"input must not produce an audit row of "+
				"its own",
		)
	}
}

// TestHandleUTXOCreatedRecycledChangeBooksItsCredit proves recycled change is
// not audit-only: no earlier message described it, so it books the credit leg
// every other wallet UTXO books.
func TestHandleUTXOCreatedRecycledChangeBooksItsCredit(t *testing.T) {
	t.Parallel()

	a, ledgerStore, auditStore := newTestActorWithAudit(t)
	ctx := t.Context()

	require.NoError(
		t,
		run(
			ctx, a,
			proceedsMsg(
				proceedsOutpoint(0x9b, 1), 12_000,
				ClassificationRecycledChange,
			),
		),
	)

	entries := ledgerStore.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, AccountWalletBalance, entries[0].DebitAccount)
	require.Equal(t, AccountOpeningBalance, entries[0].CreditAccount)
	require.Equal(t, int64(12_000), entries[0].AmountSat)

	audits := auditStore.getEntries()
	require.Len(t, audits, 1)
	require.Equal(
		t, ClassificationRecycledChange, audits[0].ClassifiedAs,
	)
}

// TestHandleUTXOCreatedLeaveProceedsIsAuditOnly proves the leave's on-chain
// output writes an audit row and no ledger leg. The VTXOSentMsg proceeds leg
// already credited wallet_balance for exactly this value.
func TestHandleUTXOCreatedLeaveProceedsIsAuditOnly(t *testing.T) {
	t.Parallel()

	a, ledgerStore, auditStore := newTestActorWithAudit(t)
	ctx := t.Context()

	require.NoError(
		t,
		run(
			ctx, a,
			proceedsMsg(
				proceedsOutpoint(0x9c, 2), 55_000,
				ClassificationLeaveProceeds,
			),
		),
	)

	require.Empty(
		t, ledgerStore.getEntries(),
		"leave proceeds must not book a second wallet_balance credit",
	)

	audits := auditStore.getEntries()
	require.Len(t, audits, 1)
	require.Equal(t, ClassificationLeaveProceeds, audits[0].ClassifiedAs)
}

// TestBoardingSweepReturnReversesOnReboard covers the coin a client gets back
// when it boards, never joins a round, and the boarding sweep returns the
// funds to its wallet. That return output is credited to wallet_balance like
// any other own-wallet proceeds, so boarding it a second time must reverse the
// first credit rather than count the same satoshis twice.
//
// Both arrival orders are exercised: the deposit can reach the ledger before
// or after the sweep confirmation that produced its input, and either side
// books the reversal when it commits second.
func TestBoardingSweepReturnReversesOnReboard(t *testing.T) {
	t.Parallel()

	orders := []struct {
		name       string
		sweepFirst bool
	}{
		{
			name:       "sweep return first",
			sweepFirst: true,
		},
		{
			name:       "deposit first",
			sweepFirst: false,
		},
	}

	for _, order := range orders {
		t.Run(order.name, func(t *testing.T) {
			t.Parallel()

			assertSweepReturnReversal(t, order.sweepFirst)
		})
	}
}

// assertSweepReturnReversal runs one sweep-return/re-board pair in the
// requested order and asserts the coins are credited exactly once.
func assertSweepReturnReversal(t *testing.T, sweepFirst bool) {
	t.Helper()

	store := newDedupLedgerStore()
	a := newTestActorWithStore(t, store)
	audit := &mockUTXOAuditStore{}
	a.cfg.UTXOAuditStore = audit
	ctx := t.Context()

	const (
		boarded   = int64(100_000)
		chainCost = int64(1_000)
		returned  = boarded - chainCost
	)

	sweepTxid := [32]byte{0xa1}
	returnPoint := wire.OutPoint{
		Hash:  chainhash.Hash(sweepTxid),
		Index: 0,
	}

	sweep := &BoardingSweepConfirmedMsg{
		Txid:         sweepTxid,
		BlockHeight:  900_000,
		ChainCostSat: chainCost,
		Inputs: []SweepInput{
			{
				Outpoint:  proceedsOutpoint(0xa0, 0),
				AmountSat: boarded,
			},
		},
		DestinationSat: returned,
	}
	deposit := depositMsg(0xa2, returned, returnPoint)

	msgs := []LedgerMsg{sweep, deposit}
	if !sweepFirst {
		msgs = []LedgerMsg{deposit, sweep}
	}
	for _, msg := range msgs {
		require.NoError(t, run(ctx, a, msg))
	}

	entries := store.getEntries()

	// Over the whole story the only value that should leave wallet_balance
	// is the sweep's chain cost: the boarded coin moves out through
	// clearing, the return credits it back, and the re-board's credit and
	// the reversal cancel. Without the reversal this reads as the returned
	// value credited twice.
	balances := netBalances(entries)
	require.Equal(
		t, returned-boarded, balances[AccountWalletBalance],
		"a re-boarded sweep return must be credited exactly once",
	)

	var reversals int
	for _, entry := range entries {
		if entry.DebitAccount != AccountOpeningBalance ||
			entry.CreditAccount != AccountWalletBalance {

			continue
		}

		reversals++
		require.Equal(t, returned, entry.AmountSat)
		require.Equal(t, EventWalletUTXOSpent, entry.EventType)
		require.Equal(
			t, classifiedUTXOIdempotencyKey(
				ClassificationDepositFunding, sweepTxid, 0,
			),
			entry.IdempotencyKey,
		)
	}
	require.Equal(t, 1, reversals)
}
