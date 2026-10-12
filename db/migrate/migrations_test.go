package migrate

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
)

// newTestMigrationLogger builds a migrationLogger writing to a buffer, with
// setLevel applied to the underlying handler to choose the threshold.
func newTestMigrationLogger(setLevel func(*btclog.DefaultHandler)) (
	*migrationLogger, *bytes.Buffer) {

	var buf bytes.Buffer
	handler := btclog.NewDefaultHandler(&buf, btclog.WithNoTimestamp())
	setLevel(handler)

	return &migrationLogger{log: btclog.NewSLogger(handler)}, &buf
}

// TestMigrationLoggerQuietAtError asserts that routine migration progress is
// not emitted when the logger threshold is raised to error.
func TestMigrationLoggerQuietAtError(t *testing.T) {
	t.Parallel()

	logger, buf := newTestMigrationLogger(
		func(h *btclog.DefaultHandler) {
			h.SetLevel(btclog.LevelError)
		},
	)
	logger.Printf("%d/u %s (%dms)\n", 18, "oor_outgoing_replay", 32)

	require.Empty(t, buf.String())
}

// TestMigrationLoggerInfo asserts that migration progress is written at info
// level, with the trailing newline trimmed.
func TestMigrationLoggerInfo(t *testing.T) {
	t.Parallel()

	logger, buf := newTestMigrationLogger(
		func(h *btclog.DefaultHandler) {
			h.SetLevel(btclog.LevelInfo)
		},
	)
	logger.Printf("%d/u %s (%dms)\n", 18, "oor_outgoing_replay", 32)

	out := buf.String()
	require.Contains(t, out, "INF")
	require.Contains(t, out, "18/u oor_outgoing_replay (32ms)")
	require.NotContains(t, out, "ERR")
	require.Equal(t, 1, bytes.Count(buf.Bytes(), []byte("\n")))
}
