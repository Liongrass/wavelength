package wallet

import (
	"context"
	"errors"
	"testing"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/ledger"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// walletWithLedgerSink constructs a minimal Ark actor shell
// with the supplied ledger sink. The fields required by
// emitUTXOCreated are ledgerSink + actorLog (via the logger
// helper), so everything else can stay nil.
func walletWithLedgerSink(sink fn.Option[ledger.Sink]) *Ark {
	return &Ark{
		ledgerSink: sink,
		actorLog:   fn.Some[btclog.Logger](btclog.Disabled),
	}
}

// TestEmitUTXOCreatedForwardsClassification confirms the
// wallet helper forwards every UTXO field verbatim and carries
// the caller-supplied classification through to the ledger
// message. The helper does not infer classifications itself;
// its only jobs are null-safety and shape translation.
func TestEmitUTXOCreatedForwardsClassification(t *testing.T) {
	t.Parallel()

	sink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg](
		"ledger-capture", 4,
	)
	a := walletWithLedgerSink(fn.Some[ledger.Sink](sink))

	utxo := &Utxo{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0x11,
			},
			Index: 7,
		},
		Amount:        btcutil.Amount(42_000),
		Confirmations: 3,
	}

	require.NoError(
		t,
		a.emitUTXOCreated(
			t.Context(), utxo, 800_123,
			ledger.ClassificationDeposit,
		),
	)

	select {
	case raw := <-sink.Messages():
		msg, ok := raw.(*ledger.UTXOCreatedMsg)
		require.True(t, ok, "expected UTXOCreatedMsg, got %T", raw)

		require.Equal(
			t, [32]byte(utxo.Outpoint.Hash), msg.OutpointHash,
		)
		require.Equal(t, uint32(7), msg.OutpointIndex)
		require.Equal(t, int64(42_000), msg.AmountSat)
		require.Equal(t, uint32(800_123), msg.BlockHeight)
		require.Equal(
			t, ledger.ClassificationDeposit, msg.Classification,
		)

	default:
		t.Fatalf("no ledger emission")
	}
}

// TestEmitUTXOCreatedNegativeHeight covers the height guard:
// the block height column is unsigned but some backends pass
// -1 for unconfirmed observations; the helper clamps to 0
// rather than wrapping to uint32 max via a direct cast.
func TestEmitUTXOCreatedNegativeHeight(t *testing.T) {
	t.Parallel()

	sink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg](
		"ledger-capture", 4,
	)
	a := walletWithLedgerSink(fn.Some[ledger.Sink](sink))

	utxo := &Utxo{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0x22,
			},
		},
		Amount: btcutil.Amount(1_000),
	}

	require.NoError(
		t,
		a.emitUTXOCreated(
			t.Context(), utxo, -1, ledger.ClassificationDeposit,
		),
	)

	raw := <-sink.Messages()
	msg, ok := raw.(*ledger.UTXOCreatedMsg)
	require.True(t, ok, "expected UTXOCreatedMsg, got %T", raw)
	require.Equal(
		t, uint32(0), msg.BlockHeight,
		"negative height must clamp to 0, not wrap",
	)
}

// TestEmitUTXOCreatedNilUTXO is a null-safety regression: a
// caller that forgets to populate utxo must not panic. The
// helper returns cleanly without touching the sink.
func TestEmitUTXOCreatedNilUTXO(t *testing.T) {
	t.Parallel()

	sink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg](
		"ledger-capture", 4,
	)
	a := walletWithLedgerSink(fn.Some[ledger.Sink](sink))

	require.NoError(
		t,
		a.emitUTXOCreated(
			t.Context(), nil, 800_000, ledger.ClassificationDeposit,
		),
	)

	select {
	case msg := <-sink.Messages():
		t.Fatalf("unexpected emission on nil utxo: %T", msg)

	default:
	}
}

// TestEmitUTXOCreatedNoSink confirms the fn.None sink path is
// a silent no-op. Used by harnesses and unit tests that do
// not register a ledger actor.
func TestEmitUTXOCreatedNoSink(t *testing.T) {
	t.Parallel()

	a := walletWithLedgerSink(fn.None[ledger.Sink]())

	utxo := &Utxo{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0x33,
			},
		},
		Amount: btcutil.Amount(500),
	}

	require.NoError(
		t,
		a.emitUTXOCreated(
			t.Context(), utxo, 100, ledger.ClassificationDeposit,
		),
	)
}

// refusingLedgerSink rejects every enqueue, standing in for a durable mailbox
// whose insert fails inside the boarding-intent transaction.
type refusingLedgerSink struct {
	err error
}

// ID satisfies actor.BaseActorRef.
func (r *refusingLedgerSink) ID() string {
	return "refusing-ledger-sink"
}

// Tell returns the configured failure.
func (r *refusingLedgerSink) Tell(context.Context, ledger.LedgerMsg) error {
	return r.err
}

// TryTell returns the configured failure.
func (r *refusingLedgerSink) TryTell(context.Context, ledger.LedgerMsg) error {
	return r.err
}

// TestProcessUtxoRefusedDepositLegLeavesUtxoUnseen proves the deposit leg and
// the boarding intent are one durable fact. seenUtxos permanently suppresses
// re-detection, and the later boarding leg debits wallet_balance
// unconditionally, so a deposit leg dropped here would leave the account
// short forever. A refused enqueue must therefore fail the whole persist and
// leave the UTXO unseen for the next tip tick.
func TestProcessUtxoRefusedDepositLegLeavesUtxoUnseen(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	pkScript, boardingAddr, outpoint, backend := newBoardingUtxoFixture(t)

	enqueueErr := errors.New("mailbox insert refused")
	store := &MockBoardingStore{}
	store.On(
		"LookupBoardingAddress", mock.Anything, pkScript,
	).Return(boardingAddr, nil)
	store.On(
		"InsertBoardingIntents", mock.Anything, mock.Anything,
	).Return(nil)

	epochChan := make(chan chainsource.BlockEpoch, 1)
	walletActor := NewArk(
		backend, store, nil, newMockChainSourceActor(epochChan), nil,
		fn.Some[ledger.Sink](
			&refusingLedgerSink{
				err: enqueueErr,
			},
		),
		btclog.Disabled,
	)
	walletActor.seenUtxos = fn.NewSet[UtxoKey]()
	walletActor.notifiers = make(map[string]notifierInfo)

	utxo := &Utxo{
		Outpoint:      outpoint,
		PkScript:      pkScript,
		Amount:        btcutil.Amount(100_000),
		Confirmations: 6,
	}
	epoch := chainsource.BlockEpoch{
		Height: 100,
		Hash: chainhash.Hash{
			0xaa,
			0xbb,
		},
	}

	require.False(
		t, walletActor.processUtxo(ctx, epoch, utxo),
		"a refused deposit leg must fail the detection",
	)
	require.False(
		t,
		walletActor.seenUtxos.Contains(
			NewUtxoKey(outpoint),
		),
		"a refused deposit leg must leave the UTXO unseen so the "+
			"next tip tick retries it",
	)

	// With a sink that accepts, the same tick's retry lands both the
	// intent and its deposit leg, and only then is the UTXO suppressed.
	sink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg]("ledger", 4)
	walletActor.ledgerSink = fn.Some[ledger.Sink](sink)

	require.True(t, walletActor.processUtxo(ctx, epoch, utxo))
	require.True(t, walletActor.seenUtxos.Contains(NewUtxoKey(outpoint)))

	raw := <-sink.Messages()
	msg, ok := raw.(*ledger.UTXOCreatedMsg)
	require.True(t, ok, "expected UTXOCreatedMsg, got %T", raw)
	require.Equal(t, ledger.ClassificationDeposit, msg.Classification)
	require.Equal(t, int64(100_000), msg.AmountSat)
}

// newBoardingUtxoFixture builds the taproot boarding address, its pkScript,
// a confirmed outpoint, and a backend mock that answers the tx/block lookups
// processUtxo makes on the detection path.
func newBoardingUtxoFixture(t *testing.T) ([]byte, *BoardingAddress,
	wire.OutPoint, *MockBoardingBackend) {

	t.Helper()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	rootHash := []byte{0xaa, 0xbb, 0xcc}
	taprootKey := txscript.ComputeTaprootOutputKey(
		clientKey.PubKey(), rootHash,
	)
	address, err := btcaddr.NewAddressTaproot(
		taprootKey.SerializeCompressed()[1:],
		&chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	pkScript, err := txscript.PayToAddrScript(address)
	require.NoError(t, err)

	boardingAddr := &BoardingAddress{
		Address: address,
		Tapscript: &waddrmgr.Tapscript{
			Type:     waddrmgr.TapscriptTypeFullTree,
			RootHash: rootHash,
		},
		KeyDesc: keychain.KeyDescriptor{
			PubKey: clientKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: 42,
				Index:  0,
			},
		},
		OperatorKey: operatorKey.PubKey(),
		ExitDelay:   144,
	}

	outpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0x11,
			0x22,
		},
	}
	tx := &wire.MsgTx{
		TxOut: []*wire.TxOut{{
			Value:    100_000,
			PkScript: pkScript,
		}},
	}
	blockHash := chainhash.Hash{0xaa, 0xbb}

	backend := &MockBoardingBackend{}
	backend.On(
		"GetTransaction", mock.Anything, outpoint.Hash,
	).Return(&TxInfo{
		Tx:          tx,
		BlockHash:   &blockHash,
		BlockHeight: 100,
	}, nil)
	backend.On("GetBlock", mock.Anything, blockHash).Return(
		&wire.MsgBlock{
			Transactions: []*wire.MsgTx{tx},
		}, nil,
	)

	return pkScript, boardingAddr, outpoint, backend
}
