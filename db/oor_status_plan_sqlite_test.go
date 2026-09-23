//go:build !test_postgres

package db

import (
	"strings"
	"testing"

	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/stretchr/testify/require"
)

// TestOORStatusQueryPlan pins indexed cursor seeks and primary-key lookups.
// Each source must seek its creation-time index before filtering and LIMIT.
func TestOORStatusQueryPlan(t *testing.T) {
	t.Parallel()
	database := NewTestDB(t)
	for _, test := range []struct {
		name  string
		query string
		args  []interface{}
		want  []string
	}{
		{
			name: "list", query: sqlc.ListOORStatus,
			args: []interface{}{
				int64(101),
				int64(100),
				make([]byte, 32),
				int32(0),
				int32(-1),
			},
			want: []string{
				"SEARCH p USING INDEX idx_oor_packages_status_cursor (created_at>? AND created_at<?)",
				"SEARCH r USING INDEX idx_oor_session_registry_status_cursor",
				"CO-ROUTINE packages", "MATERIALIZE registry",
			},
		},
		{
			name: "get", query: sqlc.GetOORStatus, args: []interface{}{
				make([]byte, 32),
			},
			want: []string{
				"SEARCH p USING INDEX sqlite_autoindex_oor_packages_1",
				"SEARCH r USING INDEX sqlite_autoindex_oor_session_registry_1",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			rows, err := database.DB.QueryContext(
				t.Context(), "EXPLAIN QUERY PLAN "+test.query,
				test.args...,
			)
			require.NoError(t, err)
			defer rows.Close()
			var plan []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(
					t, rows.Scan(
						&id, &parent, &unused, &detail,
					),
				)
				plan = append(plan, detail)
			}
			require.NoError(t, rows.Err())
			text := strings.Join(plan, "\n")
			for _, expected := range test.want {
				require.Contains(t, text, expected)
			}
			for _, detail := range plan {
				for _, source := range []string{
					"SCAN p",
					"SCAN r",
				} {
					require.NotEqual(t, source, detail)
					require.False(
						t, strings.HasPrefix(
							detail, source+" ",
						),
					)
				}
			}
		})
	}
}
