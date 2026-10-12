package ledger

import (
	"bytes"
	"encoding/hex"
	"io"
	"math"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// tlvMessage is the minimal codec surface every ledger message satisfies.
type tlvMessage interface {
	// Encode writes the durable message records in canonical order.
	Encode(io.Writer) error

	// Decode reconstructs message fields from durable record bytes.
	Decode(io.Reader) error
}

// assertRoundTrip encodes src, decodes the bytes into dst, and asserts the two
// are deeply equal. This is the core wire-stability check: every durable
// message must survive an encode/decode cycle unchanged.
func assertRoundTrip(t require.TestingT, src, dst tlvMessage) {
	var buf bytes.Buffer
	require.NoError(t, src.Encode(&buf))
	require.NoError(t, dst.Decode(bytes.NewReader(buf.Bytes())))
	require.Equal(t, src, dst)
}

// genBytes16 draws a fixed-width round identifier.
func genBytes16(rt *rapid.T) [16]byte {
	var a [16]byte
	copy(a[:], rapid.SliceOfN(rapid.Byte(), 16, 16).Draw(rt, "b16"))

	return a
}

// genBytes32 draws a fixed-width session identifier or transaction hash.
func genBytes32(rt *rapid.T) [32]byte {
	var a [32]byte
	copy(a[:], rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(rt, "b32"))

	return a
}

// genAmount draws a non-negative satoshi amount. Decode rejects values past
// MaxInt64 (a malformed payload guard), so the round-trippable domain is
// [0, MaxInt64].
func genAmount(rt *rapid.T, label string) int64 {
	return rapid.Int64Range(0, math.MaxInt64).Draw(rt, label)
}

// genNonEmptyBytes draws a non-empty byte slice, avoiding the nil-vs-empty
// ambiguity that an optional zero-length TLV field would introduce.
func genNonEmptyBytes(rt *rapid.T, label string) []byte {
	return rapid.SliceOfN(rapid.Byte(), 1, 64).Draw(rt, label)
}

// TestFeePaidMsgRoundTrip property-checks FeePaidMsg encode/decode.
func TestFeePaidMsgRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		src := &FeePaidMsg{
			RoundID:        genBytes16(rt),
			AmountSat:      genAmount(rt, "amt"),
			FeeType:        FeeTypeBoarding,
			BlockHeight:    rapid.Uint32().Draw(rt, "height"),
			IdempotencyKey: genNonEmptyBytes(rt, "idem"),
		}
		assertRoundTrip(rt, src, &FeePaidMsg{})
	})
}

// TestVTXOReceivedMsgRoundTrip property-checks VTXOReceivedMsg encode/decode.
func TestVTXOReceivedMsgRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		src := &VTXOReceivedMsg{
			OutpointHash:  genBytes32(rt),
			OutpointIndex: rapid.Uint32().Draw(rt, "idx"),
			AmountSat:     genAmount(rt, "amt"),
			Source:        SourceOOR,
			SessionID:     genBytes32(rt),
		}
		assertRoundTrip(rt, src, &VTXOReceivedMsg{})
	})
}

// TestVTXOSentMsgRoundTrip property-checks VTXOSentMsg encode/decode,
// exercising the shared tlvutil.OutPointRecord path.
func TestVTXOSentMsgRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		var h chainhash.Hash
		copy(h[:], rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(rt, "oph"))

		src := &VTXOSentMsg{
			SessionID: genBytes32(rt),
			Outpoint: wire.OutPoint{
				Hash:  h,
				Index: rapid.Uint32().Draw(rt, "opi"),
			},
			AmountSat:         genAmount(rt, "amt"),
			IdempotencyKey:    genNonEmptyBytes(rt, "idem"),
			ProceedsOwnWallet: rapid.Bool().Draw(rt, "own"),
		}
		assertRoundTrip(rt, src, &VTXOSentMsg{})
	})
}

// TestExitCostMsgRoundTrip property-checks ExitCostMsg encode/decode.
func TestExitCostMsgRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		src := &ExitCostMsg{
			OutpointHash:         genBytes32(rt),
			OutpointIndex:        rapid.Uint32().Draw(rt, "idx"),
			AmountSat:            genAmount(rt, "amt"),
			ExitCostSat:          genAmount(rt, "cost"),
			BlockHeight:          rapid.Uint32().Draw(rt, "height"),
			DestinationOwnWallet: rapid.Bool().Draw(rt, "own"),
		}
		assertRoundTrip(rt, src, &ExitCostMsg{})
	})
}

// TestUTXOCreatedMsgRoundTrip property-checks UTXOCreatedMsg encode/decode.
func TestUTXOCreatedMsgRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		src := &UTXOCreatedMsg{
			OutpointHash:   genBytes32(rt),
			OutpointIndex:  rapid.Uint32().Draw(rt, "idx"),
			AmountSat:      genAmount(rt, "amt"),
			BlockHeight:    rapid.Uint32().Draw(rt, "height"),
			Classification: ClassificationDeposit,
			FundingInputs: []wire.OutPoint{{
				Hash:  chainhash.Hash(genBytes32(rt)),
				Index: rapid.Uint32().Draw(rt, "fundingIndex"),
			}},
		}
		assertRoundTrip(rt, src, &UTXOCreatedMsg{})
	})
}

// TestUTXOSpentMsgRoundTrip property-checks UTXOSpentMsg encode/decode.
func TestUTXOSpentMsgRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		src := &UTXOSpentMsg{
			OutpointHash:   genBytes32(rt),
			OutpointIndex:  rapid.Uint32().Draw(rt, "idx"),
			AmountSat:      genAmount(rt, "amt"),
			BlockHeight:    rapid.Uint32().Draw(rt, "height"),
			Classification: ClassificationRoundFunding,
		}
		assertRoundTrip(rt, src, &UTXOSpentMsg{})
	})
}

// TestVTXOSentMsgGolden pins the exact wire bytes of a fully-populated
// VTXOSentMsg. It is the cross-version guard required by the migration: a
// previously persisted durable message (these bytes) must always decode to the
// same value, and the current encoder must still produce these exact bytes. Any
// accidental wire-format change -- including a regression in the shared
// tlvutil.OutPointRecord encoding -- breaks this test loudly.
func TestVTXOSentMsgGolden(t *testing.T) {
	t.Parallel()

	// A deterministic, fully-populated message.
	var sessionID [32]byte
	for i := range sessionID {
		sessionID[i] = byte(i + 1)
	}
	var roundID [16]byte
	for i := range roundID {
		roundID[i] = byte(0xa0 + i)
	}
	var opHash chainhash.Hash
	for i := range opHash {
		opHash[i] = byte(0x10 + i)
	}

	src := &VTXOSentMsg{
		SessionID: sessionID,
		RoundID:   roundID,
		Outpoint: wire.OutPoint{
			Hash:  opHash,
			Index: 0x04030201,
		},
		AmountSat: 0x0102030405,
	}

	const legacyGolden = "01200102030405060708090a0b0c0d0e0f1011121314151" +
		"61718191a1b1c1d1e1f20030800000001020304050510a0a1a2a" +
		"3a4a5a6a7a8a9aaabacadaeaf0724101112131415161718191a1" +
		"b1c1d1e1f202122232425262728292a2b2c2d2e2f01020304"

	var buf bytes.Buffer
	require.NoError(t, src.Encode(&buf))
	// Current main appends an empty idempotency key and false proceeds
	// flag. The original four records remain byte-identical.
	require.Equal(
		t, legacyGolden+"09000b0100",
		hex.EncodeToString(
			buf.Bytes(),
		),
	)

	// Persisted messages from before the optional records still decode.
	legacy, err := hex.DecodeString(legacyGolden)
	require.NoError(t, err)
	var got VTXOSentMsg
	require.NoError(t, got.Decode(bytes.NewReader(legacy)))
	require.Equal(t, src, &got)
}

// TestBoardingSweepConfirmedMsgRoundTrip pins the newer atomic sweep message,
// including its big-endian input indexes and amounts, across the refactor.
func TestBoardingSweepConfirmedMsgRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		input := rapid.Int64Range(2, math.MaxInt64).Draw(rt, "input")
		cost := rapid.Int64Range(1, input-1).Draw(rt, "cost")
		src := &BoardingSweepConfirmedMsg{
			Txid:         genBytes32(rt),
			BlockHeight:  rapid.Uint32().Draw(rt, "height"),
			ChainCostSat: cost,
			Inputs: []SweepInput{{
				Outpoint: wire.OutPoint{
					Hash:  chainhash.Hash(genBytes32(rt)),
					Index: rapid.Uint32().Draw(rt, "index"),
				},
				AmountSat: input,
			}},
			DestinationSat:      input - cost,
			DestinationExternal: rapid.Bool().Draw(rt, "external"),
		}
		assertRoundTrip(rt, src, &BoardingSweepConfirmedMsg{})
	})
}

// TestVTXOSentMsgInvalidOutPointLength preserves the ledger error category for
// malformed outpoint records after moving the decoder to the shared package.
func TestVTXOSentMsgInvalidOutPointLength(t *testing.T) {
	t.Parallel()

	var msg VTXOSentMsg
	err := msg.Decode(bytes.NewReader([]byte{7, 1, 0}))
	require.ErrorIs(t, err, ErrInvalidMessage)
}
