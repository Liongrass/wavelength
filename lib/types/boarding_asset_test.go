package types

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestAssetBoardingAuth binds the deposit claim while proof bytes and the
// OP_TRUE witness remain independently authenticated by the asset verifier.
func TestAssetBoardingAuth(t *testing.T) {
	t.Parallel()
	req := testJoinRoundAuthRequest(t)
	bitcoin, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)
	board := req.BoardingReqs[0]
	board.AssetRef = "assetid:asset"
	board.AssetAmount = 900
	board.AssetDigest = bytes.Repeat([]byte{1}, 32)
	board.AssetCommitmentLeafHash = bytes.Repeat([]byte{2}, 32)
	original, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)
	require.NotEqual(t, bitcoin, original)
	decoded, err := DecodeJoinRoundAuthMessage(original)
	require.NoError(t, err)
	require.Equal(t, board.AssetRef, decoded.BoardingReqs[0].AssetRef)
	require.Equal(t, board.AssetAmount, decoded.BoardingReqs[0].AssetAmount)
	require.Equal(t, board.AssetDigest, decoded.BoardingReqs[0].AssetDigest)
	require.Equal(
		t, board.AssetCommitmentLeafHash,
		decoded.BoardingReqs[0].AssetCommitmentLeafHash,
	)
	for _, mutate := range []func(*BoardingRequest){
		func(r *BoardingRequest) {
			r.AssetRef += "changed"
		},
		func(r *BoardingRequest) {
			r.AssetAmount++
		},
		func(r *BoardingRequest) {
			r.AssetDigest = bytes.Repeat([]byte{3}, 32)
		},
		func(r *BoardingRequest) {
			r.AssetCommitmentLeafHash = bytes.Repeat([]byte{4}, 32)
		},
	} {
		changed := *board
		mutate(&changed)
		req.BoardingReqs[0] = &changed
		encoded, err := JoinRoundAuthMessage(req)
		require.NoError(t, err)
		require.NotEqual(t, original, encoded)
	}
	req.BoardingReqs[0] = board
	board.AssetProof = []byte("proof")
	board.AssetWitness = [][]byte{{0x51}, {1}}
	encoded, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)
	require.Equal(t, original, encoded)
}

// TestBoardingAssetFields rejects malformed claims before signing or transport.
func TestBoardingAssetFields(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*BoardingRequest)
	}{
		{
			"no ref",
			func(b *BoardingRequest) {
				b.AssetRef = ""
			},
		},
		{
			"zero amount",
			func(b *BoardingRequest) {
				b.AssetAmount = 0
			},
		},
		{
			"short digest",
			func(b *BoardingRequest) {
				b.AssetDigest = make([]byte, 31)
			},
		},
		{
			"long digest",
			func(b *BoardingRequest) {
				b.AssetDigest = make([]byte, 33)
			},
		},
		{
			"short leaf",
			func(b *BoardingRequest) {
				b.AssetCommitmentLeafHash = make([]byte, 31)
			},
		},
		{
			"long leaf",
			func(b *BoardingRequest) {
				b.AssetCommitmentLeafHash = make([]byte, 33)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			req := testJoinRoundAuthRequest(t)
			b := req.BoardingReqs[0]
			b.AssetRef = "asset-a"
			b.AssetAmount = 1
			b.AssetDigest = make([]byte, 32)
			b.AssetCommitmentLeafHash = make([]byte, 32)
			test.mutate(b)
			_, err := JoinRoundAuthMessage(req)
			require.Error(t, err)
		})
	}
	for _, mutate := range []func(*BoardingRequest){
		func(b *BoardingRequest) {
			b.AssetAmount = 1
		},
		func(b *BoardingRequest) {
			b.AssetDigest = make([]byte, 32)
		},
		func(b *BoardingRequest) {
			b.AssetCommitmentLeafHash = make([]byte, 32)
		},
		func(b *BoardingRequest) {
			b.AssetProof = []byte{
				1,
			}
		},
		func(b *BoardingRequest) {
			b.AssetWitness = [][]byte{
				{
					1,
				},
			}
		},
	} {
		req := testJoinRoundAuthRequest(t)
		mutate(req.BoardingReqs[0])
		_, err := JoinRoundAuthMessage(req)
		require.Error(t, err)
	}
}

// TestBoardingAssetAuthRecords requires all four signed records while keeping
// the Bitcoin-only encoding identical to its historical three-record form.
func TestBoardingAssetAuthRecords(t *testing.T) {
	t.Parallel()
	b := testJoinRoundAuthRequest(t).BoardingReqs[0]
	hash, index, err := encodeJoinAuthOutPoint(b.Outpoint)
	require.NoError(t, err)
	policy, err := b.EffectivePolicyTemplate()
	require.NoError(t, err)
	base := []tlv.Record{
		tlv.MakePrimitiveRecord(
			joinRoundAuthOutPointHashRecordType, &hash,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthOutPointIndexRecordType, &index,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthBoardPolicyRecordType, &policy,
		),
	}
	historical, err := encodeJoinAuthTLV(base)
	require.NoError(t, err)
	bitcoin, err := encodeJoinAuthBoardingRequest(b)
	require.NoError(t, err)
	require.Equal(t, historical, bitcoin)

	ref := []byte("asset-a")
	amount := uint64(1)
	digest, leaf := make([]byte, 32), make([]byte, 32)
	assetRecords := []tlv.Record{
		tlv.MakePrimitiveRecord(
			joinRoundAuthBoardAssetRefRecordType, &ref,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthBoardAssetAmountRecordType, &amount,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthBoardAssetDigestRecordType, &digest,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthBoardAssetLeafRecordType, &leaf,
		),
	}
	for mask := 0; mask < 16; mask++ {
		records := append([]tlv.Record(nil), base...)
		for i, record := range assetRecords {
			if mask&(1<<i) != 0 {
				records = append(records, record)
			}
		}
		encoded, err := encodeJoinAuthTLV(records)
		require.NoError(t, err)
		decoded, err := decodeJoinAuthBoardingRequest(encoded)
		if mask != 0 && mask != 15 {
			require.ErrorContains(t, err, "must appear together")
			continue
		}
		require.NoError(t, err)
		reencoded, err := encodeJoinAuthBoardingRequest(decoded)
		require.NoError(t, err)
		require.Equal(t, encoded, reencoded)
	}

	for _, mutate := range []func(){
		func() {
			ref = nil
		},
		func() {
			ref = []byte("asset-a")
			amount = 0
		},
		func() {
			amount = 1
			digest = make([]byte, 31)
		},
		func() {
			digest = make([]byte, 32)
			leaf = make([]byte, 33)
		},
	} {
		mutate()
		records := append(
			append(
				[]tlv.Record(nil), base...,
			),
			assetRecords...,
		)
		encoded, err := encodeJoinAuthTLV(records)
		require.NoError(t, err)
		_, err = decodeJoinAuthBoardingRequest(encoded)
		require.Error(t, err)
	}
}
