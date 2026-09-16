package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/stretchr/testify/require"
)

// newCheckStore migrates a fresh sqlite database and opens it through the
// tool's own read path, so every test runs the checks against the same store
// construction an operator would get.
func newCheckStore(t *testing.T) (*db.Store, *config) {
	t.Helper()

	dbFile := filepath.Join(t.TempDir(), "waved.db")
	migrated, err := db.NewStoreFromConfig(&db.Config{
		Backend: "sqlite",
		Sqlite: &db.SqliteConfig{
			DatabaseFileName: dbFile,
			SkipMigrations:   false,
		},
		Postgres: &db.PostgresConfig{},
	}, btclog.Disabled)
	require.NoError(t, err)
	require.NoError(t, migrated.Close())

	cfg := &config{
		backend:      "sqlite",
		sqliteDBFile: dbFile,
		format:       "json",
	}

	// Reopen read-write: the checks only read, but the seeding helpers
	// below need to write through the same handle.
	store, err := db.NewStoreFromConfig(&db.Config{
		Backend: "sqlite",
		Sqlite: &db.SqliteConfig{
			DatabaseFileName: dbFile,
			SkipMigrations:   true,
		},
		Postgres: &db.PostgresConfig{},
	}, btclog.Disabled)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})

	return store, cfg
}

// insertEntry seeds one ledger entry.
func insertEntry(t *testing.T, store *db.Store,
	params sqlc.InsertClientLedgerEntryParams) {

	t.Helper()

	require.NoError(
		t,
		store.Queries().InsertClientLedgerEntry(
			context.Background(),
			params,
		),
	)
}

// insertAudit seeds one wallet UTXO audit row.
func insertAudit(t *testing.T, store *db.Store,
	params sqlc.InsertWalletUTXOLogParams) {

	t.Helper()

	require.NoError(
		t,
		store.Queries().InsertWalletUTXOLog(
			context.Background(),
			params,
		),
	)
}

// insertVTXO seeds one VTXO row at the given status. The VTXO table has no
// sqlc insert that leaves every unrelated descriptor field alone, so the test
// writes the row directly; this is test fixture setup, not production SQL.
func insertVTXO(t *testing.T, store *db.Store, hash []byte, index int32,
	amount int64, status int, spent bool) {

	t.Helper()

	// vtxos.round_id is a foreign key, so the round has to exist first.
	_, err := store.BaseDB().ExecContext(context.Background(), `
		INSERT INTO rounds (round_id, creation_time, last_update_time)
		VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING`, "round", 1, 1)
	require.NoError(t, err)

	_, err = store.BaseDB().ExecContext(context.Background(), `
		INSERT INTO vtxos (
			outpoint_hash, outpoint_index, round_id, amount,
			pk_script, expiry, operator_pubkey, batch_expiry,
			created_height, commitment_txid, spent, status,
			creation_time, last_update_time
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		hash, index, "round", amount, []byte{0x51}, 100, []byte{0x02},
		200, 1, []byte{0x03}, spent, status, 1, 1,
	)
	require.NoError(t, err)
}

// vtxoReceiveEntry builds a VTXO receive leg at the given chain identity.
func vtxoReceiveEntry(hash []byte, amount int64,
	key []byte) sqlc.InsertClientLedgerEntryParams {

	return sqlc.InsertClientLedgerEntryParams{
		DebitAccount:   ledger.AccountVTXOBalance,
		CreditAccount:  ledger.AccountTransfersIn,
		AmountSat:      amount,
		IdempotencyKey: key,
		EventType:      ledger.EventVTXOReceived,
		Description:    "test receive",
		CreatedAt:      1,
		ChainTxid:      hash,
		ChainVout: sql.NullInt32{
			Int32: 0,
			Valid: true,
		},
	}
}

// runChecks evaluates every invariant and returns the named results.
func runChecks(t *testing.T, store *db.Store,
	cfg *config) map[string]checkResult {

	t.Helper()

	rep, err := buildCheckReport(context.Background(), store, cfg)
	require.NoError(t, err)

	byName := make(map[string]checkResult, len(rep.Checks))
	for _, result := range rep.Checks {
		byName[result.Name] = result
	}

	return byName
}

// TestCheckCleanLedgerPasses seeds a consistent ledger — a deposit with its
// audit row and boarding intent, and a VTXO receive matching the live VTXO
// inventory — and verifies every check passes and the run exits zero.
func TestCheckCleanLedgerPasses(t *testing.T) {
	store, cfg := newCheckStore(t)
	ctx := context.Background()

	hash := bytes.Repeat([]byte{0xaa}, 32)
	insertVTXO(t, store, hash, 0, 5_000, 0, false)
	insertEntry(t, store, vtxoReceiveEntry(hash, 5_000, []byte("recv")))

	rep, err := buildCheckReport(ctx, store, cfg)
	require.NoError(t, err)

	for _, result := range rep.Checks {
		require.True(
			t, result.Passed, "check %s: %+v", result.Name,
			result.Findings,
		)
	}
	require.True(t, rep.Passed)
	require.Zero(t, rep.Violations)
	require.Equal(t, int64(1), rep.EntryCount)
	require.NotEmpty(t, rep.LedgerFingerprint)

	// A clean run writes its report and returns no error, so the process
	// exits zero.
	var buf bytes.Buffer
	require.NoError(t, runCheck(ctx, store, cfg, &buf))
	require.Contains(t, buf.String(), "\"passed\": true")
}

// TestCheckFingerprintIsContentOnly verifies the journal fingerprint changes
// with journal content and is otherwise stable, which is what lets an
// operator compare a restored copy against the live database.
func TestCheckFingerprintIsContentOnly(t *testing.T) {
	store, cfg := newCheckStore(t)
	ctx := context.Background()

	first, err := buildCheckReport(ctx, store, cfg)
	require.NoError(t, err)

	second, err := buildCheckReport(ctx, store, cfg)
	require.NoError(t, err)
	require.Equal(t, first.LedgerFingerprint, second.LedgerFingerprint)

	hash := bytes.Repeat([]byte{0xbb}, 32)
	insertEntry(t, store, vtxoReceiveEntry(hash, 1_000, []byte("fp")))

	third, err := buildCheckReport(ctx, store, cfg)
	require.NoError(t, err)
	require.NotEqual(t, first.LedgerFingerprint, third.LedgerFingerprint)
}

// TestCheckWalletClearingResidue seeds a sweep that debited wallet_clearing
// without the closing credit, and verifies the clearing-closure check reports
// the residue while the trial balance still closes.
func TestCheckWalletClearingResidue(t *testing.T) {
	store, cfg := newCheckStore(t)

	hash := bytes.Repeat([]byte{0xcc}, 32)
	insertAudit(t, store, sqlc.InsertWalletUTXOLogParams{
		OutpointHash:  hash,
		OutpointIndex: 0,
		AmountSat:     7_000,
		Event:         "spent",
		BlockHeight:   10,
		ClassifiedAs:  ledger.ClassificationBoardingSweepInput,
		CreatedAt:     1,
	})
	insertEntry(t, store, sqlc.InsertClientLedgerEntryParams{
		DebitAccount:   ledger.AccountWalletClearing,
		CreditAccount:  ledger.AccountWalletBalance,
		AmountSat:      7_000,
		IdempotencyKey: []byte("sweep-input"),
		EventType:      ledger.EventWalletUTXOSpent,
		Description:    "test sweep input",
		CreatedAt:      1,
		ChainTxid:      hash,
		ChainVout:      sql.NullInt32{Int32: 0, Valid: true},
	})

	results := runChecks(t, store, cfg)
	require.True(t, results[checkTrialBalance].Passed)
	require.False(t, results[checkClearingClosure].Passed)
	require.Len(t, results[checkClearingClosure].Findings, 1)
	require.Equal(
		t, "7000",
		results[checkClearingClosure].Findings[0].Fields["balance_sat"],
	)
}

// TestCheckVTXOInventoryMismatch seeds a ledger receive with no matching live
// VTXO row and verifies the inventory check reports the difference.
func TestCheckVTXOInventoryMismatch(t *testing.T) {
	store, cfg := newCheckStore(t)

	hash := bytes.Repeat([]byte{0xdd}, 32)
	insertEntry(t, store, vtxoReceiveEntry(hash, 9_000, []byte("orphan")))

	results := runChecks(t, store, cfg)
	require.False(t, results[checkVTXOInventory].Passed)
	require.Len(t, results[checkVTXOInventory].Findings, 1)

	fields := results[checkVTXOInventory].Findings[0].Fields
	require.Equal(t, "9000", fields["ledger_vtxo_balance_sat"])
	require.Equal(t, "0", fields["live_vtxo_sum_sat"])

	// A terminal VTXO does not count towards the live inventory, so
	// seeding one as Spent (4) leaves the mismatch in place.
	insertVTXO(t, store, hash, 0, 9_000, 4, true)
	results = runChecks(t, store, cfg)
	require.False(t, results[checkVTXOInventory].Passed)

	// Flipping it to Live closes the gap.
	_, err := store.BaseDB().ExecContext(context.Background(),
		"UPDATE vtxos SET status = 0, spent = FALSE")
	require.NoError(t, err)

	results = runChecks(t, store, cfg)
	require.True(t, results[checkVTXOInventory].Passed)
}

// TestCheckWalletAuditReconciliation seeds a deposit ledger leg whose audit
// row carries a different amount and verifies the reconciliation reports it.
func TestCheckWalletAuditReconciliation(t *testing.T) {
	store, cfg := newCheckStore(t)

	hash := bytes.Repeat([]byte{0xee}, 32)
	insertAudit(t, store, sqlc.InsertWalletUTXOLogParams{
		OutpointHash:  hash,
		OutpointIndex: 0,
		AmountSat:     1_000,
		Event:         "created",
		BlockHeight:   10,
		ClassifiedAs:  ledger.ClassificationChange,
		CreatedAt:     1,
	})
	insertEntry(t, store, sqlc.InsertClientLedgerEntryParams{
		DebitAccount:   ledger.AccountWalletBalance,
		CreditAccount:  ledger.AccountOpeningBalance,
		AmountSat:      2_000,
		IdempotencyKey: []byte("deposit"),
		EventType:      ledger.EventWalletUTXOCreated,
		Description:    "test deposit",
		CreatedAt:      1,
		ChainTxid:      hash,
		ChainVout:      sql.NullInt32{Int32: 0, Valid: true},
	})

	results := runChecks(t, store, cfg)
	require.False(t, results[checkWalletReconcile].Passed)
	require.Len(t, results[checkWalletReconcile].Findings, 1)

	fields := results[checkWalletReconcile].Findings[0].Fields
	require.Equal(t, "1000", fields["audit_created_sat"])
	require.Equal(t, "2000", fields["ledger_created_sat"])
	require.Contains(
		t, results[checkWalletReconcile].Note, "audit-log-derived",
	)
}

// TestCheckAuditPairing seeds an audit row with no ledger leg and a ledger leg
// with no audit row, and verifies both directions are reported while an
// audit-only spent classification is left alone.
func TestCheckAuditPairing(t *testing.T) {
	store, cfg := newCheckStore(t)

	// Audit-only classification: a round-funding spend books no leg, so
	// it must not be reported.
	insertAudit(t, store, sqlc.InsertWalletUTXOLogParams{
		OutpointHash:  bytes.Repeat([]byte{0x01}, 32),
		OutpointIndex: 0,
		AmountSat:     500,
		Event:         "spent",
		BlockHeight:   10,
		ClassifiedAs:  ledger.ClassificationRoundFunding,
		CreatedAt:     1,
	})

	// Ledger-emitting spend with no leg.
	insertAudit(t, store, sqlc.InsertWalletUTXOLogParams{
		OutpointHash:  bytes.Repeat([]byte{0x02}, 32),
		OutpointIndex: 0,
		AmountSat:     600,
		Event:         "spent",
		BlockHeight:   10,
		ClassifiedAs:  ledger.ClassificationBoardingSweepInput,
		CreatedAt:     1,
	})

	// Ledger leg with no audit row.
	insertEntry(t, store, sqlc.InsertClientLedgerEntryParams{
		DebitAccount:   ledger.AccountWalletBalance,
		CreditAccount:  ledger.AccountOpeningBalance,
		AmountSat:      700,
		IdempotencyKey: []byte("unpaired"),
		EventType:      ledger.EventWalletUTXOCreated,
		Description:    "test unpaired deposit",
		CreatedAt:      1,
		ChainTxid:      bytes.Repeat([]byte{0x03}, 32),
		ChainVout:      sql.NullInt32{Int32: 0, Valid: true},
	})

	results := runChecks(t, store, cfg)
	require.False(t, results[checkAuditPairing].Passed)
	require.Len(t, results[checkAuditPairing].Findings, 2)

	details := []string{
		results[checkAuditPairing].Findings[0].Detail,
		results[checkAuditPairing].Findings[1].Detail,
	}
	require.Contains(t, details, "audit row has no ledger leg")
	require.Contains(t, details, "ledger leg has no audit row")
}

// TestCheckOutpointLegAmounts seeds an exit whose send and proceeds legs
// disagree in amount and verifies the leg-agreement check reports it.
func TestCheckOutpointLegAmounts(t *testing.T) {
	store, cfg := newCheckStore(t)

	var hash [32]byte
	copy(hash[:], bytes.Repeat([]byte{0x0f}, 32))

	insertEntry(t, store, sqlc.InsertClientLedgerEntryParams{
		DebitAccount:   ledger.AccountTransfersOut,
		CreditAccount:  ledger.AccountVTXOBalance,
		AmountSat:      4_000,
		IdempotencyKey: ledger.ExitSendIdempotencyKey(hash, 0),
		EventType:      ledger.EventVTXOSent,
		Description:    "test exit send",
		CreatedAt:      1,
	})
	insertEntry(t, store, sqlc.InsertClientLedgerEntryParams{
		DebitAccount:   ledger.AccountWalletBalance,
		CreditAccount:  ledger.AccountTransfersOut,
		AmountSat:      3_900,
		IdempotencyKey: ledger.ExitProceedsIdempotencyKey(hash, 0),
		EventType:      ledger.EventVTXOSent,
		Description:    "test exit proceeds",
		CreatedAt:      1,
	})

	results := runChecks(t, store, cfg)
	require.False(t, results[checkLegAmounts].Passed)
	require.Len(t, results[checkLegAmounts].Findings, 1)

	fields := results[checkLegAmounts].Findings[0].Fields
	require.Equal(t, "unilateral_exit", fields["operation"])
	require.Equal(t, "4000", fields["send_sat"])
	require.Equal(t, "3900", fields["proceeds_sat"])
}

// TestCheckExitSendFollowedFlagAnomaly seeds the pre-fix exit send leg whose
// debit account followed the destination flag, plus its correctly-shaped
// twin, and verifies the anomaly is reported as a double credit.
func TestCheckExitSendFollowedFlagAnomaly(t *testing.T) {
	store, cfg := newCheckStore(t)

	hash := bytes.Repeat([]byte{0x11}, 32)
	vout := sql.NullInt32{Int32: 0, Valid: true}

	insertEntry(t, store, sqlc.InsertClientLedgerEntryParams{
		DebitAccount:   ledger.AccountWalletBalance,
		CreditAccount:  ledger.AccountVTXOBalance,
		AmountSat:      4_000,
		IdempotencyKey: []byte("legacy-flag-following"),
		EventType:      ledger.EventVTXOSent,
		Description:    "pre-fix exit send",
		CreatedAt:      1,
		ChainTxid:      hash,
		ChainVout:      vout,
	})
	insertEntry(t, store, sqlc.InsertClientLedgerEntryParams{
		DebitAccount:   ledger.AccountTransfersOut,
		CreditAccount:  ledger.AccountVTXOBalance,
		AmountSat:      4_000,
		IdempotencyKey: []byte("post-fix-twin"),
		EventType:      ledger.EventVTXOSent,
		Description:    "post-fix exit send",
		CreatedAt:      1,
		ChainTxid:      hash,
		ChainVout:      vout,
	})

	results := runChecks(t, store, cfg)
	require.False(t, results[anomalyExitSendFlag].Passed)
	require.Len(t, results[anomalyExitSendFlag].Findings, 1)
	require.Contains(
		t, results[anomalyExitSendFlag].Findings[0].Detail,
		"credited twice",
	)
}

// TestCheckDepositBoardingIntentGap seeds a deposit-classified audit row with
// no boarding intent and verifies the anomaly is reported.
func TestCheckDepositBoardingIntentGap(t *testing.T) {
	store, cfg := newCheckStore(t)

	insertAudit(t, store, sqlc.InsertWalletUTXOLogParams{
		OutpointHash:  bytes.Repeat([]byte{0x12}, 32),
		OutpointIndex: 1,
		AmountSat:     8_000,
		Event:         "created",
		BlockHeight:   10,
		ClassifiedAs:  ledger.ClassificationDeposit,
		CreatedAt:     1,
	})

	results := runChecks(t, store, cfg)
	require.False(t, results[anomalyDepositIntentGap].Passed)
	require.Len(t, results[anomalyDepositIntentGap].Findings, 1)
	require.Contains(
		t, results[anomalyDepositIntentGap].Findings[0].Detail,
		"no boarding intent",
	)
}

// TestCheckExitsNonZeroOnViolation verifies runCheck returns the sentinel
// error that makes the process exit non-zero, and still writes the report.
func TestCheckExitsNonZeroOnViolation(t *testing.T) {
	store, cfg := newCheckStore(t)
	cfg.format = "text"

	insertEntry(
		t, store,
		vtxoReceiveEntry(
			bytes.Repeat(
				[]byte{0x13}, 32,
			),
			1_000,
			[]byte("orphan"),
		),
	)

	var buf bytes.Buffer
	err := runCheck(context.Background(), store, cfg, &buf)
	require.True(t, errors.Is(err, errCheckFailed))
	require.Contains(t, buf.String(), "FAIL vtxo_balance_inventory")
	require.Contains(t, buf.String(), "FAILED:")
}

// TestSplitSubcommand verifies the subcommand peeling keeps flag-only
// invocations on the historical report path.
func TestSplitSubcommand(t *testing.T) {
	name, rest := splitSubcommand(nil)
	require.Equal(t, subcommandReport, name)
	require.Empty(t, rest)

	name, rest = splitSubcommand([]string{"--format", "json"})
	require.Equal(t, subcommandReport, name)
	require.Equal(t, []string{"--format", "json"}, rest)

	name, rest = splitSubcommand([]string{"check", "--format", "json"})
	require.Equal(t, subcommandCheck, name)
	require.Equal(t, []string{"--format", "json"}, rest)
}

// TestParseLedgerKey verifies the versioned key parser splits operation, leg
// and payload, and rejects unversioned keys.
func TestParseLedgerKey(t *testing.T) {
	var hash [32]byte
	copy(hash[:], bytes.Repeat([]byte{0x14}, 32))

	operation, leg, payload, ok := parseLedgerKey(
		ledger.ExitProceedsIdempotencyKey(hash, 7),
	)
	require.True(t, ok)
	require.Equal(t, "unilateral_exit", operation)
	require.Equal(t, legNameProceeds, leg)
	require.NotEmpty(t, payload)

	_, _, _, ok = parseLedgerKey([]byte("plain-outpoint-key"))
	require.False(t, ok)
}

// TestCheckRecycledProceedsClassifications verifies the checker knows the two
// classifications the recycled-proceeds lifecycle adds: an exit-proceeds
// audit row books no ledger leg by design and must not be reported as
// unpaired or counted in the wallet reconciliation, while a deposit-funding
// spend does book one and is reported when it is missing.
func TestCheckRecycledProceedsClassifications(t *testing.T) {
	store, cfg := newCheckStore(t)

	// Exit proceeds: audit-only by design.
	insertAudit(t, store, sqlc.InsertWalletUTXOLogParams{
		OutpointHash:  bytes.Repeat([]byte{0x21}, 32),
		OutpointIndex: 0,
		AmountSat:     95_000,
		Event:         "created",
		BlockHeight:   10,
		ClassifiedAs:  ledger.ClassificationExitProceeds,
		CreatedAt:     1,
	})

	results := runChecks(t, store, cfg)
	require.True(t, results[checkAuditPairing].Passed)
	require.True(
		t, results[checkWalletReconcile].Passed,
		"exit proceeds must not count towards the created total",
	)

	// Deposit funding: a ledger-emitting spend, so a missing leg is a
	// violation.
	insertAudit(t, store, sqlc.InsertWalletUTXOLogParams{
		OutpointHash:  bytes.Repeat([]byte{0x21}, 32),
		OutpointIndex: 0,
		AmountSat:     95_000,
		Event:         "spent",
		BlockHeight:   11,
		ClassifiedAs:  ledger.ClassificationDepositFunding,
		CreatedAt:     2,
	})

	results = runChecks(t, store, cfg)
	require.False(t, results[checkAuditPairing].Passed)
	require.Len(t, results[checkAuditPairing].Findings, 1)
	require.Equal(
		t, "audit row has no ledger leg",
		results[checkAuditPairing].Findings[0].Detail,
	)
}

// insertBoardingIntent seeds one boarding intent at a confirmation height.
// The intent's foreign keys mean its address has to exist first, so the test
// writes both rows directly; this is fixture setup, not production SQL.
func insertBoardingIntent(t *testing.T, store *db.Store, hash []byte,
	index int32, amount int64, confHeight int32, status string) {

	t.Helper()

	ctx := context.Background()
	pkScript := append([]byte{0x51, 0x20}, hash[:8]...)

	_, err := store.BaseDB().ExecContext(ctx, `
		INSERT INTO boarding_addresses (
			pk_script, address_string, operator_pubkey,
			exit_delay, creation_time
		) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`,
		pkScript, "bcrt1qtest", []byte{0x03}, 144, 1,
	)
	require.NoError(t, err)

	_, err = store.BaseDB().ExecContext(ctx, `
		INSERT INTO boarding_intents (
			outpoint_hash, outpoint_index, pk_script, amount,
			conf_height, conf_hash, status, creation_time,
			last_update_time
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		hash, index, pkScript, amount, confHeight, []byte{0x05},
		status, 1, 1,
	)
	require.NoError(t, err)
}

// TestCheckVTXOInventoryToleratesAnInFlightExit proves the inventory check
// does not cry wolf on a healthy database.
//
// A VTXO flips to UnilateralExit at exit *initiation*, but the leg that
// removes its value from vtxo_balance is driven only once the sweep confirms,
// a full CSV delay later. For that whole window the ledger legitimately holds
// value the live inventory does not, and the check must report it as a
// reconciling item rather than a violation. A Failed VTXO is the same shape
// permanently, since nothing ever removes its value.
func TestCheckVTXOInventoryToleratesAnInFlightExit(t *testing.T) {
	store, cfg := newCheckStore(t)

	live := bytes.Repeat([]byte{0xa1}, 32)
	exiting := bytes.Repeat([]byte{0xa2}, 32)
	failed := bytes.Repeat([]byte{0xa3}, 32)

	insertVTXO(t, store, live, 0, 5_000, 0, false)
	insertVTXO(t, store, exiting, 0, 7_000, 5, false)
	insertVTXO(t, store, failed, 0, 3_000, 6, false)

	// The ledger still carries all three: the exit's removing leg has not
	// run, and the failed VTXO has no removing leg at all.
	insertEntry(t, store, vtxoReceiveEntry(live, 15_000, []byte("recv")))

	results := runChecks(t, store, cfg)
	require.True(
		t, results[checkVTXOInventory].Passed, "an in-flight exit "+
			"is a reconciling item, not a violation: %+v",
		results[checkVTXOInventory].Findings,
	)
	require.Contains(t, results[checkVTXOInventory].Note, "exiting=7000")
	require.Contains(t, results[checkVTXOInventory].Note, "failed=3000")

	// The band still has a floor: value below the synchronous inventory
	// is a real disagreement.
	_, err := store.BaseDB().ExecContext(context.Background(),
		"UPDATE ledger_entries SET amount_sat = 1000")
	require.NoError(t, err)

	results = runChecks(t, store, cfg)
	require.False(t, results[checkVTXOInventory].Passed)
}

// TestCheckIntentGapSkipsThePreLedgerEra proves the depositless-intent
// direction does not report intents confirmed before the accounting schema
// existed. boarding_intents arrives in migration 2 and the ledger in
// migration 6, so an upgraded database holds intents that could never have
// had a deposit leg.
func TestCheckIntentGapSkipsThePreLedgerEra(t *testing.T) {
	store, cfg := newCheckStore(t)

	// An intent from before any audit row was ever written.
	insertBoardingIntent(
		t, store,
		bytes.Repeat(
			[]byte{0xb1}, 32,
		),
		0,
		6_000,
		100, "confirmed",
	)

	// With an empty audit log there is no ledger era to compare against,
	// so nothing is reported at all.
	results := runChecks(t, store, cfg)
	require.True(t, results[anomalyDepositIntentGap].Passed)

	// The ledger's own history starts at height 500. The old intent stays
	// below the floor.
	deposit := bytes.Repeat([]byte{0xb2}, 32)
	insertBoardingIntent(t, store, deposit, 0, 6_000, 500, "confirmed")
	insertAudit(t, store, sqlc.InsertWalletUTXOLogParams{
		OutpointHash:  deposit,
		OutpointIndex: 0,
		AmountSat:     6_000,
		Event:         "created",
		BlockHeight:   500,
		ClassifiedAs:  ledger.ClassificationDeposit,
		CreatedAt:     1,
	})
	insertEntry(t, store, sqlc.InsertClientLedgerEntryParams{
		DebitAccount:   ledger.AccountWalletBalance,
		CreditAccount:  ledger.AccountOpeningBalance,
		AmountSat:      6_000,
		IdempotencyKey: []byte("deposit"),
		EventType:      ledger.EventWalletUTXOCreated,
		Description:    "test deposit",
		CreatedAt:      1,
		ChainTxid:      deposit,
		ChainVout:      sql.NullInt32{Int32: 0, Valid: true},
	})

	results = runChecks(t, store, cfg)
	require.True(
		t, results[anomalyDepositIntentGap].Passed, "a pre-ledger "+
			"intent is not a violation: %+v",
		results[anomalyDepositIntentGap].Findings,
	)

	// An intent confirmed inside the ledger era with no deposit leg still
	// is one, whatever its lifecycle status: every intent row was written
	// at confirmation, so a later status does not excuse a missing leg.
	insertBoardingIntent(
		t, store,
		bytes.Repeat(
			[]byte{0xb3}, 32,
		),
		0,
		4_000,
		900, "adopted",
	)

	results = runChecks(t, store, cfg)
	require.False(t, results[anomalyDepositIntentGap].Passed)
	require.Len(t, results[anomalyDepositIntentGap].Findings, 1)
}

// TestCheckAuditPairingMatchesAmounts proves a pair that agrees on chain
// identity but disagrees on value is a finding, not a silent pass.
func TestCheckAuditPairingMatchesAmounts(t *testing.T) {
	store, cfg := newCheckStore(t)

	hash := bytes.Repeat([]byte{0xc1}, 32)
	insertAudit(t, store, sqlc.InsertWalletUTXOLogParams{
		OutpointHash:  hash,
		OutpointIndex: 0,
		AmountSat:     6_000,
		Event:         "created",
		BlockHeight:   500,
		ClassifiedAs:  ledger.ClassificationChange,
		CreatedAt:     1,
	})
	insertEntry(t, store, sqlc.InsertClientLedgerEntryParams{
		DebitAccount:   ledger.AccountWalletBalance,
		CreditAccount:  ledger.AccountOpeningBalance,
		AmountSat:      5_000,
		IdempotencyKey: []byte("mismatched"),
		EventType:      ledger.EventWalletUTXOCreated,
		Description:    "test deposit",
		CreatedAt:      1,
		ChainTxid:      hash,
		ChainVout:      sql.NullInt32{Int32: 0, Valid: true},
	})

	results := runChecks(t, store, cfg)
	require.False(t, results[checkAuditPairing].Passed)
}

// TestCheckClearingClosureCarriesTheInFlightCaveat proves the check tells an
// operator that a residue on a live daemon may just be an unconfirmed sweep.
func TestCheckClearingClosureCarriesTheInFlightCaveat(t *testing.T) {
	store, cfg := newCheckStore(t)

	results := runChecks(t, store, cfg)
	require.True(t, results[checkClearingClosure].Passed)
	require.Contains(
		t, results[checkClearingClosure].Note, "in flight",
	)
	require.Contains(
		t, results[checkTrialBalance].Note, "weak evidence",
	)
}

// TestCheckIntentGapIgnoresSentinelHeights proves a height-0 audit row does
// not erase the pre-ledger floor. Producers write 0 for a height they do not
// know yet, and one such row taken as a real height would drag the minimum to
// the bottom of the range and report every pre-ledger intent as a violation.
func TestCheckIntentGapIgnoresSentinelHeights(t *testing.T) {
	store, cfg := newCheckStore(t)

	// An intent from before the ledger era, with no deposit leg.
	insertBoardingIntent(
		t, store,
		bytes.Repeat(
			[]byte{0xe1}, 32,
		),
		0,
		6_000,
		100, "confirmed",
	)

	// The ledger era starts at height 900, and a later row lands with an
	// unknown height.
	insertAudit(t, store, sqlc.InsertWalletUTXOLogParams{
		OutpointHash:  bytes.Repeat([]byte{0xe2}, 32),
		OutpointIndex: 0,
		AmountSat:     6_000,
		Event:         "created",
		BlockHeight:   900,
		ClassifiedAs:  ledger.ClassificationChange,
		CreatedAt:     1,
	})
	insertAudit(t, store, sqlc.InsertWalletUTXOLogParams{
		OutpointHash:  bytes.Repeat([]byte{0xe3}, 32),
		OutpointIndex: 0,
		AmountSat:     2_000,
		Event:         "created",
		BlockHeight:   0,
		ClassifiedAs:  ledger.ClassificationSweepReturn,
		CreatedAt:     1,
	})

	results := runChecks(t, store, cfg)
	require.True(
		t, results[anomalyDepositIntentGap].Passed, "a sentinel "+
			"height must not lower the ledger-era floor: %+v",
		results[anomalyDepositIntentGap].Findings,
	)
}

// roundSendEntry builds a round-level directed-send leg. It carries a round
// id but no chain identity, which is exactly the shape the history query
// surfaces with an empty txid and a -1 output index.
func roundSendEntry(roundID []byte, amount int64,
	key []byte) sqlc.InsertClientLedgerEntryParams {

	return sqlc.InsertClientLedgerEntryParams{
		DebitAccount:   ledger.AccountTransfersOut,
		CreditAccount:  ledger.AccountVTXOBalance,
		AmountSat:      amount,
		RoundID:        roundID,
		IdempotencyKey: key,
		EventType:      ledger.EventVTXOSent,
		Description:    "test round send",
		CreatedAt:      1,
	}
}

// TestCheckHistoryDistinctSeparatesRounds proves two equal-amount directed
// sends of the same subtype in different rounds are not reported as a
// duplicate. Round-level history rows share an empty txid and the -1 output
// index sentinel, so a key built from the chain columns alone would collide
// two independent movements and fail a healthy ledger.
func TestCheckHistoryDistinctSeparatesRounds(t *testing.T) {
	store, cfg := newCheckStore(t)

	insertEntry(
		t, store,
		roundSendEntry(
			bytes.Repeat(
				[]byte{0x01}, 32,
			),
			5_000,
			[]byte("send-round-1"),
		),
	)
	insertEntry(
		t, store,
		roundSendEntry(
			bytes.Repeat(
				[]byte{0x02}, 32,
			),
			5_000,
			[]byte("send-round-2"),
		),
	)

	result := runChecks(t, store, cfg)[checkHistoryDistinct]
	require.True(t, result.Passed, "findings: %+v", result.Findings)
	require.Empty(t, result.Findings)
}

// TestCheckHistoryDistinctFlagsARepeatedRound proves the check still catches
// a real duplicate: two identically shaped sends inside one round, which a
// consumer would render as the same movement twice.
func TestCheckHistoryDistinctFlagsARepeatedRound(t *testing.T) {
	store, cfg := newCheckStore(t)

	roundID := bytes.Repeat([]byte{0x03}, 32)
	insertEntry(t, store, roundSendEntry(roundID, 5_000, []byte("dup-a")))
	insertEntry(t, store, roundSendEntry(roundID, 5_000, []byte("dup-b")))

	result := runChecks(t, store, cfg)[checkHistoryDistinct]
	require.False(t, result.Passed)
	require.Len(t, result.Findings, 1)
	require.Equal(
		t, "duplicate transaction history row",
		result.Findings[0].Detail,
	)
}
