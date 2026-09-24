package waved

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// TestOORSessionSummaryToProtoProjectsRetryReason verifies the RPC projection
// exposes a pending retry reason without misclassifying it as terminal, while
// preserving the same field for a terminal failure.
func TestOORSessionSummaryToProtoProjectsRetryReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		summary    oor.SessionSummary
		wantStatus waverpc.OORSessionStatus
	}{
		{
			name: "pending retry",
			summary: oor.SessionSummary{
				SessionID: oor.SessionID(chainhash.Hash{1}),
				Direction: oor.SessionDirectionOutgoing,
				Phase: string(
					oor.OutgoingPhaseSubmitSent,
				),
				Pending:     true,
				RetryReason: "input not spendable",
			},
			wantStatus: waverpc.
				OORSessionStatus_OOR_SESSION_STATUS_PENDING,
		},
		{
			name: "terminal failure",
			summary: oor.SessionSummary{
				SessionID:   oor.SessionID(chainhash.Hash{2}),
				Direction:   oor.SessionDirectionOutgoing,
				Phase:       string(oor.OutgoingPhaseFailed),
				RetryReason: "retry budget exhausted",
			},
			wantStatus: waverpc.
				OORSessionStatus_OOR_SESSION_STATUS_FAILED,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			info := oorSessionSummaryToProto(test.summary)
			require.Equal(t, test.wantStatus, info.GetStatus())
			require.Equal(
				t, test.summary.RetryReason,
				info.GetFailureReason(),
			)
		})
	}
}

// TestPageOORSessionsUsesCreationCursor pins the timestamp and ID boundary
// encoded from the final returned row, excluding the lookahead row.
func TestPageOORSessionsUsesCreationCursor(t *testing.T) {
	t.Parallel()
	id := chainhash.Hash{1}
	sessions := []*waverpc.OORSessionInfo{
		{
			SessionId: chainhash.Hash{
				2,
			}.String(),
			CreatedAt: 200,
		},
		{
			SessionId: id.String(),
			CreatedAt: 100,
		},
		{
			SessionId: chainhash.Hash{
				3,
			}.String(),
			CreatedAt: 99,
		},
	}
	page, token := pageOORSessions(sessions, 2)
	require.Equal(t, sessions[:2], page)
	cursor, err := decodeOORStatusCursor(token)
	require.NoError(t, err)
	require.EqualValues(t, 100, cursor.CreatedAt)
	require.Equal(t, id, cursor.SessionID)
	page, token = pageOORSessions(sessions[2:], 2)
	require.Equal(t, sessions[2:], page)
	require.Empty(t, token)
}

// TestOORSessionMatchesFilters checks direction and status filtering.
func TestOORSessionMatchesFilters(t *testing.T) {
	t.Parallel()

	outgoing := waverpc.
		OORSessionDirection_OOR_SESSION_DIRECTION_OUTGOING
	incoming := waverpc.
		OORSessionDirection_OOR_SESSION_DIRECTION_INCOMING
	pending := waverpc.OORSessionStatus_OOR_SESSION_STATUS_PENDING

	info := &waverpc.OORSessionInfo{
		Direction: outgoing,
		Status:    pending,
	}

	require.True(
		t,
		oorSessionMatchesFilters(
			info, &waverpc.ListOORSessionsRequest{},
		),
	)

	require.True(
		t,
		oorSessionMatchesFilters(
			info, &waverpc.ListOORSessionsRequest{
				DirectionFilter: outgoing,
				StatusFilter:    pending,
			},
		),
	)

	require.False(
		t,
		oorSessionMatchesFilters(
			info, &waverpc.ListOORSessionsRequest{
				DirectionFilter: incoming,
			},
		),
	)
}

// TestParseOORSessionIDNormalizesCase verifies uppercase session ids are
// accepted and normalized to the canonical chainhash string form.
func TestParseOORSessionIDNormalizesCase(t *testing.T) {
	t.Parallel()

	const sessionID = "ABCDEF0123456789ABCDEF0123456789" +
		"ABCDEF0123456789ABCDEF0123456789"
	const normalizedID = "abcdef0123456789abcdef0123456789" +
		"abcdef0123456789abcdef0123456789"

	hash, err := parseOORSessionID(sessionID)
	require.NoError(t, err)
	require.Equal(t, normalizedID, hash.String())
}

// TestRoundInfoMatchesFilters checks state and creation-time filtering.
func TestRoundInfoMatchesFilters(t *testing.T) {
	t.Parallel()

	info := &waverpc.RoundInfo{
		State:        waverpc.RoundState_ROUND_STATE_CONFIRMED,
		CreationTime: 100,
	}

	require.True(
		t,
		roundInfoMatchesFilters(
			info, &waverpc.ListRoundsRequest{},
		),
	)

	require.True(
		t,
		roundInfoMatchesFilters(
			info, &waverpc.ListRoundsRequest{
				StateFilter: waverpc.
					RoundState_ROUND_STATE_CONFIRMED,
				CreatedAfter: 50,
			},
		),
	)

	require.False(
		t,
		roundInfoMatchesFilters(
			info, &waverpc.ListRoundsRequest{
				StateFilter: waverpc.
					RoundState_ROUND_STATE_FAILED,
			},
		),
	)

	require.False(
		t,
		roundInfoMatchesFilters(
			info, &waverpc.ListRoundsRequest{
				CreatedBefore: 50,
			},
		),
	)
}
