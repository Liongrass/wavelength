package db

import (
	"math"
	"os"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/round"
	"github.com/stretchr/testify/require"
)

// TestRoundStoreOutputProvenance verifies the signature checkpoint preserves
// the accounting origin and refresh source of every requested output. The
// confirmation path uses the restored origin to emit the correct ledger legs.
func TestRoundStoreOutputProvenance(t *testing.T) {
	t.Parallel()

	for _, origin := range []types.VTXOOrigin{
		types.VTXOOriginUnknown,
		types.VTXOOriginRoundBoarding,
		types.VTXOOriginRoundRefresh,
		types.VTXOOriginRoundTransfer,
		types.VTXOOriginAutoRefresh,
	} {
		t.Run(origin.String(), func(t *testing.T) {
			t.Parallel()

			store, _ := newRoundStoreForTest(t)
			r := createTestRound(t, testRoundIDDB(origin.String()))
			owned := createTestClientVTXO(t, r.RoundID, 1)
			for i := range 3 {
				request := types.VTXORequest{
					Amount:         owned.Amount,
					PolicyTemplate: owned.PolicyTemplate,
					OwnerKey:       owned.OwnerKey,
					Origin:         origin,
				}
				if origin == types.VTXOOriginRoundRefresh ||
					origin == types.VTXOOriginAutoRefresh {

					source := &wire.OutPoint{
						Hash: chainhash.Hash{
							byte(i + 1),
						},
						Index: uint32(i),
					}
					if i == 2 {
						source.Index = math.MaxUint32
					}
					request.RefreshSourceOutpoint = source
				}
				r.Intents.VTXOs = append(
					r.Intents.VTXOs, request,
				)
			}

			state := &round.InputSigSentState{
				RoundID: r.RoundID,
				Intents: r.Intents,
				ClientTrees: make(
					map[round.SignerKey]*tree.Tree,
				),
			}
			require.NoError(
				t,
				store.CommitState(
					t.Context(), r, state,
				),
			)

			_, restored, err := store.FetchState(
				t.Context(), r.RoundID,
			)
			require.NoError(t, err)
			checkpoint, ok := restored.(*round.InputSigSentState)
			require.True(t, ok)
			require.Len(t, checkpoint.Intents.VTXOs, 3)
			for i, request := range checkpoint.Intents.VTXOs {
				require.Equal(t, origin, request.Origin)
				want := r.Intents.VTXOs[i].RefreshSourceOutpoint
				require.Equal(
					t, want, request.RefreshSourceOutpoint,
				)
			}
		})
	}
}

// TestRoundOutputProvenanceMigration preserves legacy requests without
// inventing their origin or refresh pairing, including a down/up cycle.
func TestRoundOutputProvenanceMigration(t *testing.T) {
	t.Parallel()

	database := NewTestDBWithVersion(t, 23)
	fixture, err := os.ReadFile(
		"testdata/round_output_provenance_legacy.sql",
	)
	require.NoError(t, err)
	_, err = database.ExecContext(
		t.Context(),
		transformByteLiterals(
			t, database.BaseDB, string(fixture),
		),
	)
	require.NoError(t, err)

	for range 2 {
		require.NoError(t, database.ExecuteMigrations(TargetLatest))
		rows, err := database.GetRoundVtxoRequests(
			t.Context(),
			"00000000-0000-0000-0000-000000000024",
		)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.EqualValues(t, 12_000, rows[0].Amount)
		require.Equal(t, []byte{0x51, 0x20, 0x01}, rows[0].PkScript)
		require.EqualValues(t, types.VTXOOriginUnknown, rows[0].Origin)
		require.Empty(t, rows[0].RefreshSourceHash)
		require.False(t, rows[0].RefreshSourceIndex.Valid)
		require.NoError(
			t,
			database.ExecuteMigrations(
				TargetVersion(23),
			),
		)
	}
}
