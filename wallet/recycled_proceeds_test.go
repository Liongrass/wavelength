package wallet

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/ledger"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// stubOwnedScriptSet recognises an explicit set of scripts.
type stubOwnedScriptSet struct {
	scripts map[string]struct{}
}

// IsOwnedWalletScript reports membership in the set.
func (s stubOwnedScriptSet) IsOwnedWalletScript(_ context.Context,
	pkScript []byte) (bool, error) {

	_, ok := s.scripts[string(pkScript)]

	return ok, nil
}

// recycledHarness wires an Ark shell with a capturing ledger sink and a
// scripted owned-script set.
func recycledHarness(t *testing.T,
	owned ...[]byte) (*Ark, *actor.ChannelTellOnlyRef[ledger.LedgerMsg]) {

	t.Helper()

	sink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg](
		"ledger-capture", 8,
	)
	a := walletWithLedgerSink(fn.Some[ledger.Sink](sink))

	scripts := make(map[string]struct{}, len(owned))
	for _, script := range owned {
		scripts[string(script)] = struct{}{}
	}
	a.ownedScripts = stubOwnedScriptSet{scripts: scripts}

	return a, sink
}

// drain collects every message the sink received.
func drain(
	sink *actor.ChannelTellOnlyRef[ledger.LedgerMsg]) []ledger.LedgerMsg {

	var msgs []ledger.LedgerMsg
	for {
		select {
		case msg := <-sink.Messages():
			msgs = append(msgs, msg)

		default:
			return msgs
		}
	}
}

// TestFundingInputsReportEveryPreviousOutpoint proves the wallet reports the
// funding transaction's inputs verbatim, without judging which of them might
// be ours. That judgement belongs to the ledger actor, which is the only side
// that can make it without depending on message ordering.
func TestFundingInputsReportEveryPreviousOutpoint(t *testing.T) {
	t.Parallel()

	first := wire.OutPoint{Hash: chainhash.Hash{0xa1}, Index: 0}
	second := wire.OutPoint{Hash: chainhash.Hash{0xa2}, Index: 7}

	tx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: first,
			},
			nil,
			{
				PreviousOutPoint: second,
			},
		},
	}

	require.Equal(
		t, []wire.OutPoint{first, second}, fundingInputs(tx),
	)
	require.Nil(t, fundingInputs(nil))
}

// TestRecycledProceedsCreditsChange proves a partial spend credits back the
// change output paying a script the daemon minted, under the classification
// the ledger's reversal recognises. The ledger reverses the whole input, so
// without this credit the client is understated by the change.
func TestRecycledProceedsCreditsChange(t *testing.T) {
	t.Parallel()

	changeScript := []byte{0x51, 0x20, 0xcc}
	a, sink := recycledHarness(t, changeScript)

	fundingTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: wire.OutPoint{
					Hash: chainhash.Hash{
						0xa3,
					},
				},
			},
		},
		TxOut: []*wire.TxOut{
			{
				Value: 20_000,
				PkScript: []byte{
					0x51,
					0x20,
					0xbb,
				},
			},
			{
				Value:    29_000,
				PkScript: changeScript,
			},
		},
	}
	deposit := wire.OutPoint{Hash: fundingTx.TxHash(), Index: 0}

	require.NoError(
		t,
		a.emitRecycledProceedsChange(
			t.Context(), fundingTx, deposit, 900_400,
		),
	)

	msgs := drain(sink)
	require.Len(t, msgs, 1)

	created, ok := msgs[0].(*ledger.UTXOCreatedMsg)
	require.True(t, ok)
	require.Equal(t, [32]byte(fundingTx.TxHash()), created.OutpointHash)
	require.Equal(t, uint32(1), created.OutpointIndex)
	require.Equal(t, int64(29_000), created.AmountSat)
	require.Equal(
		t, ledger.ClassificationRecycledChange, created.Classification,
	)
	require.Equal(t, uint32(900_400), created.BlockHeight)
}

// TestRecycledProceedsSkipsForeignOutputs proves only outputs paying a script
// the registry knows are credited, and never the boarding output itself,
// whose value the deposit leg already books.
func TestRecycledProceedsSkipsForeignOutputs(t *testing.T) {
	t.Parallel()

	boardingScript := []byte{0x51, 0x20, 0xbb}

	// The boarding output's own script is in the registry, which is the
	// case that would double-credit if the deposit outpoint were not
	// skipped explicitly.
	a, sink := recycledHarness(t, boardingScript)

	fundingTx := &wire.MsgTx{
		TxOut: []*wire.TxOut{
			{
				Value:    20_000,
				PkScript: boardingScript,
			},
			{
				Value: 29_000,
				PkScript: []byte{
					0x51,
					0x20,
					0xdd,
				},
			},
		},
	}
	deposit := wire.OutPoint{Hash: fundingTx.TxHash(), Index: 0}

	require.NoError(
		t,
		a.emitRecycledProceedsChange(
			t.Context(), fundingTx, deposit, 900_500,
		),
	)
	require.Empty(t, drain(sink))
}

// TestRecycledProceedsNeedsTheRegistry proves the change credit is skipped
// when the owned-script registry is not wired. That understates the client,
// which is the safe direction, and it cannot compound: an uncredited change
// output has no credit for a later board of it to reverse.
func TestRecycledProceedsNeedsTheRegistry(t *testing.T) {
	t.Parallel()

	sink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg](
		"ledger-capture", 4,
	)
	a := walletWithLedgerSink(fn.Some[ledger.Sink](sink))

	fundingTx := &wire.MsgTx{
		TxOut: []*wire.TxOut{
			{
				Value: 1_000,
				PkScript: []byte{
					0x51,
				},
			},
		},
	}

	require.NoError(
		t,
		a.emitRecycledProceedsChange(
			t.Context(), fundingTx, wire.OutPoint{}, 1,
		),
	)
	require.Empty(t, drain(sink))
}
