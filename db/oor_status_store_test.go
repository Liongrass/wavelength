package db

import (
	"bytes"
	"context"
	"database/sql"
	"sort"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// statusQueryCounter records reads so history growth cannot silently add
// checkpoint queries, unbounded package queries, or per-history-row reads.
type statusQueryCounter struct {
	sqlc.DBTX
	queries *[]string
}

// QueryContext records a multi-row read before passing it to the transaction.
func (c *statusQueryCounter) QueryContext(ctx context.Context, query string,
	args ...interface{}) (*sql.Rows, error) {

	*c.queries = append(*c.queries, query)

	return c.DBTX.QueryContext(ctx, query, args...)
}

// QueryRowContext records a point read before passing it to the transaction.
func (c *statusQueryCounter) QueryRowContext(ctx context.Context, query string,
	args ...interface{}) *sql.Row {

	*c.queries = append(*c.queries, query)

	return c.DBTX.QueryRowContext(ctx, query, args...)
}

// TestOORStatusPagination pins creation-time ordering, disjoint source merge,
// filter-before-limit, migration of existing rows and bounded payload reads.
// Both database backends run this test through the test_postgres build tag.
func TestOORStatusPagination(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	database := NewTestDBWithVersion(t, 24)
	var ids []chainhash.Hash
	for i := 0; i < 257; i++ {
		// IDs deliberately disagree with timestamp ordering, and raw
		// byte order differs from the displayed hash used by old
		// cursors.
		id := chainhash.Hash{
			byte(256 - i),
			17: byte(i),
			30: byte(i),
			31: byte(i / 256),
		}
		ids = append(ids, id)
		if i%2 == 0 {
			_, err := database.UpsertOORPackage(
				ctx, sqlc.UpsertOORPackageParams{
					SessionID: id[:],
					Direction: int32(i % 4 / 2),
					ArkPsbt: []byte(
						"not a PSBT: status must " +
							"never parse this",
					),
					CreatedAt: int64(500 + i/5),
					UpdatedAt: int64(
						999 - i,
					),
				},
			)
			require.NoError(t, err)
			require.NoError(
				t,
				database.InsertOORPackageCheckpoint(
					ctx,
					sqlc.InsertOORPackageCheckpointParams{
						SessionID: id[:],
						CheckpointPsbt: []byte(
							"not a checkpoint",
						),
					},
				),
			)
		}
		if i%6 == 0 {
			continue
		}
		// Most packages also have a conflicting incoming registry row,
		// as with outgoing self-change. Packages must win before
		// filters.
		registryDirection := int32(2)
		if i == 4 || i == 5 {
			registryDirection = 1
		}
		require.NoError(
			t,
			database.UpsertOORSessionRegistry(
				ctx, sqlc.UpsertOORSessionRegistryParams{
					SessionID: id[:],
					ActorID:   id.String(),
					Direction: registryDirection,
					SnapshotData: []byte(
						"diagnostic snapshot",
					),
					Phase:     "ReceiveNotified",
					Status:    int32(i % 3),
					CreatedAt: int64(100 + i/5),
					UpdatedAt: int64(i + 1),
				},
			),
		)
	}
	require.NoError(t, database.ExecuteMigrations(TargetLatest))
	createdAt := func(id chainhash.Hash) int64 {
		i := int(id[30]) + int(id[31])*256
		if i%6 == 0 {
			return int64(500 + i/5)
		}

		return int64(100 + i/5)
	}
	sort.Slice(ids, func(i, j int) bool {
		if createdAt(ids[i]) != createdAt(ids[j]) {
			return createdAt(ids[i]) > createdAt(ids[j])
		}

		return bytes.Compare(ids[i][:], ids[j][:]) > 0
	})
	var queries []string
	store := &OORStatusStore{NewTransactionExecutor(database.BaseDB,
		func(tx *sql.Tx) *sqlc.Queries {
			counter := &statusQueryCounter{
				DBTX:    tx,
				queries: &queries,
			}
			if database.Backend() == sqlc.BackendTypePostgres {
				return sqlc.NewPostgres(counter)
			}

			return sqlc.NewSqlite(counter)
		}, btclog.Disabled,
	)}
	for _, filter := range []struct {
		direction OORSessionDirection
		status    int32
	}{
		{
			0,
			-1,
		}, {
			1,
			-1,
		}, {
			2,
			-1,
		}, {
			0,
			0,
		}, {
			0,
			1,
		}, {
			0,
			2,
		}, {
			1,
			0,
		}, {
			2,
			1,
		}, {
			99,
			-1,
		},
	} {
		var expected []string
		for _, id := range ids {
			i := int(id[30]) + int(id[31])*256
			dir, state := int32(2), int32(i%3)
			if i == 5 {
				dir = 1
			}
			if i%2 == 0 {
				dir, state = int32(2-i%4/2), 1
			}
			if filter.direction != 0 &&
				int32(filter.direction) != dir {

				continue
			}
			if filter.status != -1 && filter.status != state {
				continue
			}
			expected = append(expected, id.String())
		}
		var got []string
		var after *OORStatusCursor
		for {
			queries = nil
			page, err := store.List(
				ctx, after, filter.direction, filter.status, 7,
			)
			require.NoError(t, err)
			require.LessOrEqual(t, len(page), 7)
			require.LessOrEqual(t, len(queries), 1+2*len(page))
			for _, query := range queries {
				require.Contains(t, []string{
					sqlc.ListOORStatus,
					sqlc.ListOORVTXOBindingsBySession,
					sqlc.GetOOROutgoingStatusSnapshot,
				}, query)
			}
			for _, entry := range page {
				id := chainhash.Hash(entry.Metadata.SessionID)
				got = append(got, id.String())
				require.Equal(
					t, createdAt(id),
					entry.Metadata.CreatedAt,
				)
				// Only outgoing registry rows may hydrate a
				// snapshot, even when package metadata has a
				// different direction.
				i := int(entry.Metadata.SessionID[30]) +
					int(entry.Metadata.SessionID[31])*256
				if i == 4 || i == 5 {
					require.NotNil(t, entry.Registry)
					require.Equal(
						t, []byte(
							"diagnostic snapshot",
						),
						entry.Registry.SnapshotData,
					)
				} else {
					require.Nil(t, entry.Registry)
				}
			}
			if len(page) < 7 {
				break
			}
			last := page[len(page)-1].Metadata
			after = &OORStatusCursor{
				CreatedAt: last.CreatedAt,
				SessionID: chainhash.Hash(last.SessionID),
			}
		}
		require.Equal(t, expected, got, "filter %+v", filter)
	}
	// Point reads use primary keys and never scan either history source.
	queries = nil
	entry, err := store.Get(ctx, ids[128])
	require.NoError(t, err)
	require.Equal(t, ids[128][:], entry.Metadata.SessionID)
	require.LessOrEqual(t, len(queries), 3)
	require.Equal(t, sqlc.GetOORStatus, queries[0])
	queries = nil
	_, err = store.Get(ctx, chainhash.Hash{31: 255})
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.Equal(t, []string{sqlc.GetOORStatus}, queries)
	// A cursor includes its timestamp and needs no surviving source row.
	cursor := &OORStatusCursor{
		CreatedAt: 130, SessionID: chainhash.Hash{
			0xff,
		},
	}
	_, err = store.Get(ctx, cursor.SessionID)
	require.ErrorIs(t, err, sql.ErrNoRows)
	page, err := store.List(ctx, cursor, 0, -1, 1)
	require.NoError(t, err)
	for _, id := range ids {
		sameTime := createdAt(id) == cursor.CreatedAt
		beforeID := bytes.Compare(id[:], cursor.SessionID[:]) < 0
		if createdAt(id) > cursor.CreatedAt ||
			(sameTime && !beforeID) {

			continue
		}
		require.Equal(t, id[:], page[0].Metadata.SessionID)

		break
	}
	_, err = store.List(ctx, nil, 0, -1, 0)
	require.Error(t, err)
}

// TestOORStatusBindings verifies incoming outputs and outgoing inputs survive
// the narrow status projection and package bindings suppress snapshot loading.
func TestOORStatusBindings(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	database := NewTestDB(t)
	root := NewStore(
		database.DB, database.Queries, database.Backend(),
		btclog.Disabled,
	)
	artifacts := root.NewOORArtifactStore(clock.NewDefaultClock())
	rounds := root.NewRoundStore(
		&chaincfg.RegressionNetParams, clock.NewDefaultClock(),
	)
	id, ark, checkpoints, output, script, amount, _ := buildTestOORPackage(
		t, 0x51,
	)
	require.NoError(
		t, artifacts.UpsertPackage(
			ctx, OORPackageDirectionOutgoing, id, ark, checkpoints,
		),
	)
	seedBindingOutpoint(t, ctx, rounds, output, script, amount)
	require.NoError(
		t, artifacts.UpsertBinding(
			ctx, output, id, 0, OORPackageLinkKindCreatedOutput,
		),
	)
	input := wire.OutPoint{Hash: chainhash.Hash{3}, Index: 1}
	seedBindingOutpoint(t, ctx, rounds, input, script, amount)
	require.NoError(
		t, artifacts.UpsertBinding(
			ctx, input, id, 0, OORPackageLinkKindConsumedInput,
		),
	)
	// Authoritative consumed bindings make the registry snapshot
	// unnecessary even when its direction differs from the package.
	require.NoError(
		t,
		database.UpsertOORSessionRegistry(
			ctx, sqlc.UpsertOORSessionRegistryParams{
				SessionID: id[:],
				ActorID:   id.String(),
				Direction: 2,
				Status:    0,
				Phase:     "irrelevant",
			},
		),
	)
	result, err := NewOORStatusStore(root).Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, []wire.OutPoint{input}, result.ConsumedOutpoints)
	require.Equal(t, []wire.OutPoint{output}, result.CreatedOutpoints)
	require.Nil(t, result.Registry)
	require.EqualValues(t, 1, result.Metadata.Direction)
	require.EqualValues(t, 1, result.Metadata.Status)
}

// TestOORStatusPackageCutoff protects the package lower bound: full registry
// pages may discard older packages, but must retain ties. Partial and empty
// filtered registry pages must still find older package-only sessions.
func TestOORStatusPackageCutoff(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	database := NewTestDB(t)
	for _, row := range []struct {
		id        byte
		createdAt int64
		direction int32
	}{
		{
			10,
			100,
			1,
		}, {
			20,
			90,
			1,
		}, {
			30,
			80,
			2,
		},
	} {
		id := chainhash.Hash{row.id}
		require.NoError(
			t,
			database.UpsertOORSessionRegistry(
				ctx, sqlc.UpsertOORSessionRegistryParams{
					SessionID: id[:],
					ActorID:   id.String(),
					Direction: row.direction,
					CreatedAt: row.createdAt,
				},
			),
		)
	}
	for _, row := range []struct {
		id        byte
		createdAt int64
		direction int32
	}{
		// The overlapping package's later date must not set the floor.
		{
			10,
			500,
			1,
		}, {
			40,
			90,
			1,
		}, {
			50,
			89,
			1,
		}, {
			60,
			-10,
			0,
		},
	} {
		id := chainhash.Hash{row.id}
		_, err := database.UpsertOORPackage(
			ctx, sqlc.UpsertOORPackageParams{
				SessionID: id[:], Direction: row.direction,
				CreatedAt: row.createdAt, ArkPsbt: []byte(
					"unused",
				),
			},
		)
		require.NoError(t, err)
	}
	store := NewOORStatusStore(
		NewStore(
			database.DB, database.Queries, database.Backend(),
			btclog.Disabled,
		),
	)
	for _, test := range []struct {
		name      string
		before    *OORStatusCursor
		direction OORSessionDirection
		status    int32
		limit     int64
		want      []byte
	}{
		{
			name: "full page with tie", status: -1,
			limit: 2, want: []byte{
				10,
				40,
			},
		},
		{
			name:      "partial filtered page",
			direction: 2, status: -1, limit: 2, want: []byte{
				30,
				60,
			},
		},
		{
			name:      "empty filtered registry",
			direction: 2, status: 1, limit: 2, want: []byte{
				60,
			},
		},
		{
			name: "exhausted sources", direction: 1,
			status: -1, limit: 5, want: []byte{
				10,
				40,
				20,
				50,
			},
		},
		{
			name: "continuation", status: -1,
			before: &OORStatusCursor{
				CreatedAt: 100, SessionID: chainhash.Hash{
					10,
				},
			},
			limit: 2, want: []byte{
				40,
				20,
			},
		},
		{
			name: "cursor within tie", status: -1,
			before: &OORStatusCursor{
				CreatedAt: 90, SessionID: chainhash.Hash{
					40,
				},
			},
			limit: 2, want: []byte{
				20,
				50,
			},
		},
		{
			name:   "partial continuation",
			status: -1, before: &OORStatusCursor{
				CreatedAt: 90, SessionID: chainhash.Hash{
					20,
				},
			},
			limit: 2, want: []byte{
				50,
				30,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			page, err := store.List(
				ctx, test.before, test.direction, test.status,
				test.limit,
			)
			require.NoError(t, err)
			var got []byte
			for _, entry := range page {
				got = append(got, entry.Metadata.SessionID[0])
			}
			require.Equal(t, test.want, got)
		})
	}
}
