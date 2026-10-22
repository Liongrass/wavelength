//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"
	"testing"

	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

// batchWaitRPC supplies the optional live round status to activity tests.
type batchWaitRPC struct {
	*fakeRPCServer
	rounds []*waverpc.RoundInfo
}

// ListRounds returns a detached test snapshot of the selected batch window.
func (f *batchWaitRPC) ListRounds(context.Context, *waverpc.ListRoundsRequest) (
	*waverpc.ListRoundsResponse, error) {

	return &waverpc.ListRoundsResponse{Rounds: f.rounds}, nil
}

// TestActivityBatchWaitReconciliation exercises real persistence and replay:
// selecting, repeating, rescheduling, and clearing a wait update the same row.
func TestActivityBatchWaitReconciliation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	runtime, store, rpc := newReconcileFixture(t)
	const address = "bcrt1qbatchwait"
	const id = "deposit-" + address
	rpc.listTxResp = &waverpc.ListTransactionsResponse{
		Transactions: []*waverpc.TransactionHistoryEntry{{
			Type:               "boarding",
			Subtype:            ledger.EventWalletUTXOCreated,
			ConfirmationStatus: "pending", AmountSat: 100000,
			Txid: "boarding-txid", BoardingAddress: address,
			CreatedAtUnixS: 100,
		}},
	}
	info := &waverpc.RoundInfo{
		RoundKey: "pending",
		State:    waverpc.RoundState_ROUND_STATE_PENDING_ASSEMBLY,
		BoardingAddresses: []string{
			address,
		},
		WaitingReason: "Waiting for scheduled batch registration",
		BatchWindow: &waverpc.ScheduledBatchWindow{
			RegistrationOpensUnix: 1800000000,
			CutoffUnix:            1800000060,
			PlannedJoinUnixMs:     1800000012500,
		},
	}
	provider := &batchWaitRPC{
		fakeRPCServer: rpc,
		rounds: []*waverpc.RoundInfo{
			info,
		},
	}
	runtime.deps.RPCServer = provider
	h := newHistory(runtime.deps, runtime)

	runtime.reconcileActivity(ctx)
	got := listedEntry(t, h, id)
	require.Equal(
		t, info.WaitingReason, got.GetProgress().GetWaitingReason(),
	)
	require.Equal(
		t, int64(1800000060),
		got.GetProgress().GetBatchWindow().GetCutoffUnix(),
	)
	events, err := store.PullEvents(ctx, 0, 100)
	require.NoError(t, err)
	require.Len(t, events, 1)
	replay := &wavewalletrpc.WalletEntry{}
	require.NoError(
		t,
		protojson.Unmarshal(
			[]byte(events[0].EntryJson), replay,
		),
	)
	require.Equal(
		t, got.GetProgress().GetBatchWindow(),
		replay.GetProgress().GetBatchWindow(),
	)

	runtime.reconcileActivity(ctx)
	events, err = store.PullEvents(ctx, 0, 100)
	require.NoError(t, err)
	require.Len(t, events, 1)

	// A later attempt can select a different slot without changing the
	// payment's ID or status. This must append exactly one new event.
	info.BatchWindow.CutoffUnix += 3600
	info.BatchWindow.RegistrationOpensUnix += 3600
	info.BatchWindow.PlannedJoinUnixMs += 3600000
	runtime.reconcileActivity(ctx)
	events, err = store.PullEvents(ctx, 0, 100)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(
		t, int64(1800003660),
		listedEntry(t, h, id).GetProgress().GetBatchWindow().
			GetCutoffUnix(),
	)

	// The selected wait is no longer active after joining or restarting.
	provider.rounds = nil
	runtime.reconcileActivity(ctx)
	got = listedEntry(t, h, id)
	require.Nil(t, got.GetProgress().GetBatchWindow())
	require.Empty(t, got.GetProgress().GetWaitingReason())
	require.Equal(
		t, wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING, got.Status,
	)
	events, err = store.PullEvents(ctx, 0, 100)
	require.NoError(t, err)
	require.Len(t, events, 3)
	runtime.reconcileActivity(ctx)
	events, err = store.PullEvents(ctx, 0, 100)
	require.NoError(t, err)
	require.Len(t, events, 3)
}

// TestActivityBatchWaitCorrelation annotates only the operations participating
// in the waiting round, never unrelated deposits, exits, or terminal rows.
func TestActivityBatchWaitCorrelation(t *testing.T) {
	t.Parallel()
	runtime, _, rpc := newReconcileFixture(t)
	info := &waverpc.RoundInfo{
		BoardingAddresses: []string{
			"deposit-address",
		},
		InputOutpoints: []string{
			"input:0",
		},
		WaitingReason: "Waiting for scheduled batch registration",
		BatchWindow: &waverpc.ScheduledBatchWindow{
			CutoffUnix: 1800000060,
		},
	}
	runtime.deps.RPCServer = &batchWaitRPC{
		fakeRPCServer: rpc, rounds: []*waverpc.RoundInfo{
			info,
		},
	}
	leave := leaveEntryStub(
		"leave-job", []string{
			"input:0",
		}, "dest",
		1000, "",
		false,
	)
	unrelated := leaveEntryStub(
		"other-job", []string{
			"other:0",
		}, "dest",
		1000, "",
		false,
	)
	unilateral := unilateralExitEntryStub("input:0")
	terminal := leaveEntryStub(
		"done", []string{
			"input:0",
		}, "dest",
		1000, "",
		false,
	)
	terminal.Status = wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE
	h := newHistory(runtime.deps, runtime)
	require.NoError(
		t,
		h.applyBatchWaits(
			t.Context(), []*wavewalletrpc.WalletEntry{
				leave, unrelated, unilateral, terminal,
			},
		),
	)
	require.NotNil(t, leave.GetProgress().GetBatchWindow())
	for _, entry := range []*wavewalletrpc.WalletEntry{
		unrelated,
		unilateral,
		terminal,
	} {
		require.Nil(t, entry.GetProgress().GetBatchWindow())
	}
}

// TestEarliestActivityBatchWait keeps grouped deposit timing stable when the
// actor snapshot returns overlapping address matches in a different order.
func TestEarliestActivityBatchWait(t *testing.T) {
	t.Parallel()
	earlyWindow := &waverpc.ScheduledBatchWindow{
		CutoffUnix: 100, PlannedJoinUnixMs: 90000,
	}
	earlier := &waverpc.RoundInfo{BatchWindow: earlyWindow}
	later := &waverpc.RoundInfo{BatchWindow: &waverpc.ScheduledBatchWindow{
		CutoffUnix: 200, PlannedJoinUnixMs: 190000,
	}}
	for _, rounds := range [][]*waverpc.RoundInfo{
		{
			earlier,
			later,
		}, {
			later,
			earlier,
		},
	} {
		waits := make(map[string]*waverpc.RoundInfo)
		for _, info := range rounds {
			rememberEarliestWait(waits, "address", info)
		}
		require.Same(t, earlier, waits["address"])
	}
}
