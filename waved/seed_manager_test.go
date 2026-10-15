package waved

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/aezeed"
	"github.com/stretchr/testify/require"
)

// TestGenerateSeedProduces24Words verifies that GenerateSeed returns a
// valid 24-word mnemonic.
func TestGenerateSeedProduces24Words(t *testing.T) {
	mnemonic, err := GenerateSeed(nil)
	require.NoError(t, err)

	for i, word := range mnemonic {
		require.NotEmpty(t, word, "word %d is empty", i)
	}
}

// TestMnemonicToSeedRoundtrip verifies that a mnemonic can be decoded
// back to the same seed entropy.
func TestMnemonicToSeedRoundtrip(t *testing.T) {
	passphrase := []byte("test-passphrase")

	mnemonic, err := GenerateSeed(passphrase)
	require.NoError(t, err)

	seed, err := MnemonicToSeed(mnemonic, passphrase)
	require.NoError(t, err)
	require.NotEqual(t, [rawSeedLen]byte{}, seed)

	// Decode again to verify determinism.
	seed2, err := MnemonicToSeed(mnemonic, passphrase)
	require.NoError(t, err)
	require.Equal(t, seed, seed2)
}

// TestMnemonicToSeedWithBirthdayRoundtrip verifies that aezeed birthday
// metadata is preserved when deriving the raw btcwallet seed.
func TestMnemonicToSeedWithBirthdayRoundtrip(t *testing.T) {
	passphrase := []byte("test-passphrase")

	mnemonic, err := GenerateSeed(passphrase)
	require.NoError(t, err)

	seed, birthday, err := MnemonicToSeedWithBirthday(
		mnemonic, passphrase,
	)
	require.NoError(t, err)
	require.NotEqual(t, [rawSeedLen]byte{}, seed)
	require.False(t, birthday.IsZero())
	require.WithinDuration(t, time.Now(), birthday, 48*time.Hour)

	seed2, birthday2, err := MnemonicToSeedWithBirthday(
		mnemonic, passphrase,
	)
	require.NoError(t, err)
	require.Equal(t, seed, seed2)
	require.Equal(t, birthday, birthday2)
}

// TestMnemonicToSeedWrongPassphrase verifies that decoding with the
// wrong passphrase fails.
func TestMnemonicToSeedWrongPassphrase(t *testing.T) {
	mnemonic, err := GenerateSeed([]byte("correct"))
	require.NoError(t, err)

	_, err = MnemonicToSeed(mnemonic, []byte("wrong"))
	require.Error(t, err)
}

// TestLoadSeedFromEnv verifies the env var loading path.
func TestLoadSeedFromEnv(t *testing.T) {
	// Unset first — should return nil, nil.
	t.Setenv(seedEnvVar, "")
	seed, err := LoadSeedFromEnv()
	require.NoError(t, err)
	require.Nil(t, seed)

	// Set a valid 32-byte hex seed.
	hexSeed := "01020304050607080910111213141516" +
		"17181920212223242526272829303132"
	t.Setenv(seedEnvVar, hexSeed)
	seed, err = LoadSeedFromEnv()
	require.NoError(t, err)
	require.NotNil(t, seed)
	require.NotEqual(t, [rawSeedLen]byte{}, *seed)

	// Invalid hex should return an error.
	t.Setenv(seedEnvVar, "not-hex")
	_, err = LoadSeedFromEnv()
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid hex seed")

	// Wrong length should return an error.
	t.Setenv(seedEnvVar, "0102030405")
	_, err = LoadSeedFromEnv()
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid seed length")
}

// TestEnvSeedWalletPassword verifies the env-seed path only falls back to
// the public dev password where that is acceptable: silently on local test
// networks, with a warning on public ones, and on mainnet only after an
// explicit allow-insecure-mainnet opt-in.
func TestEnvSeedWalletPassword(t *testing.T) {
	tests := []struct {
		name          string
		network       string
		allowInsecure bool
		envPassword   string
		wantPassword  []byte
		wantErr       string
		wantWarn      bool
	}{
		{
			name:    "mainnet refused",
			network: "mainnet",
			wantErr: "allow-insecure-mainnet",
		},
		{
			name:          "mainnet allowed with override",
			network:       "mainnet",
			allowInsecure: true,
			wantPassword:  devWalletPassword,
			wantWarn:      true,
		},
		{
			name:         "mainnet env password",
			network:      "mainnet",
			envPassword:  "operator-password",
			wantPassword: []byte("operator-password"),
		},
		{
			name:         "signet warns",
			network:      "signet",
			wantPassword: devWalletPassword,
			wantWarn:     true,
		},
		{
			name:         "regtest silent",
			network:      "regtest",
			wantPassword: devWalletPassword,
		},
		{
			name:         "simnet silent",
			network:      "simnet",
			wantPassword: devWalletPassword,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(walletPasswordEnvVar, tc.envPassword)

			var logs bytes.Buffer
			log := btclog.NewSLogger(
				btclog.NewDefaultHandler(&logs),
			)

			password, err := envSeedWalletPassword(
				t.Context(), log, tc.network, tc.allowInsecure,
			)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.Nil(t, password)
				require.Empty(t, logs.String())

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.wantPassword, password)

			if !tc.wantWarn {
				require.Empty(t, logs.String())

				return
			}

			require.Contains(
				t, logs.String(),
				"insecure dev password",
			)
		})
	}
}

// TestLoadPasswordFromFile verifies reading passwords from files.
func TestLoadPasswordFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "password.txt")

	// With trailing newline.
	err := os.WriteFile(path, []byte("my-password\n"), 0600)
	require.NoError(t, err)

	password, err := LoadPasswordFromFile(path)
	require.NoError(t, err)
	require.Equal(t, []byte("my-password"), password)

	// Without trailing newline.
	err = os.WriteFile(path, []byte("clean-pass"), 0600)
	require.NoError(t, err)

	password, err = LoadPasswordFromFile(path)
	require.NoError(t, err)
	require.Equal(t, []byte("clean-pass"), password)
}

// TestWalletSeedFromMnemonic verifies the InitWallet derivation path:
// the raw seed matches direct mnemonic decoding and the aezeed
// birthday that bounds btcwallet recovery rescans is preserved.
func TestWalletSeedFromMnemonic(t *testing.T) {
	password := []byte("workflow-password")
	passphrase := []byte("optional-passphrase")

	mnemonic, err := GenerateSeed(passphrase)
	require.NoError(t, err)

	seed, birthday, err := WalletSeedFromMnemonic(
		mnemonic[:], passphrase, password,
	)
	require.NoError(t, err)
	require.NotEqual(t, [rawSeedLen]byte{}, seed)
	require.False(t, birthday.IsZero())
	require.WithinDuration(t, time.Now(), birthday, 48*time.Hour)

	// The derived seed must match decoding the mnemonic directly,
	// so a wallet restored from the recorded words reproduces the
	// same keys.
	var recoveredMnemonic aezeed.Mnemonic
	copy(recoveredMnemonic[:], mnemonic[:])

	mnemonicSeed, err := MnemonicToSeed(recoveredMnemonic, passphrase)
	require.NoError(t, err)
	require.Equal(t, seed, mnemonicSeed)
}

// TestWalletSeedFromMnemonicValidation verifies the mnemonic word count
// and minimum password length checks.
func TestWalletSeedFromMnemonicValidation(t *testing.T) {
	passphrase := []byte("optional-passphrase")

	mnemonic, err := GenerateSeed(passphrase)
	require.NoError(t, err)

	// Too few mnemonic words.
	_, _, err = WalletSeedFromMnemonic(
		mnemonic[:12], passphrase, []byte("workflow-password"),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "mnemonic must be")

	// Password below the minimum length.
	_, _, err = WalletSeedFromMnemonic(
		mnemonic[:], passphrase, []byte("short"),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "at least")

	// Wrong seed passphrase fails decoding.
	_, _, err = WalletSeedFromMnemonic(
		mnemonic[:], []byte("wrong"), []byte("workflow-password"),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid mnemonic")
}

// TestLoadSeedBirthdayFromEnv covers the birthday override for the raw
// dev/CI seed. Unset must stay the zero time, which btcwallet reads as
// "scan from genesis": slow, but it never hides funds. A malformed
// value must be an error rather than a silent fall back to that same
// zero time, since a typo would otherwise degrade quietly into a full
// chain scan.
func TestLoadSeedBirthdayFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    time.Time
		wantErr bool
	}{{
		name:  "unset scans from genesis",
		value: "",
		want:  time.Time{},
	}, {
		name:  "calendar date",
		value: "2024-05-03",
		want:  time.Date(2024, 5, 3, 0, 0, 0, 0, time.UTC),
	}, {
		name:  "rfc3339",
		value: "2024-05-03T23:11:00Z",
		want:  time.Date(2024, 5, 3, 23, 11, 0, 0, time.UTC),
	}, {
		name:  "surrounding whitespace tolerated",
		value: "  2024-05-03  ",
		want:  time.Date(2024, 5, 3, 0, 0, 0, 0, time.UTC),
	}, {
		name:    "malformed is an error, not a silent genesis scan",
		value:   "May 3rd 2024",
		wantErr: true,
	}, {
		name:    "unix timestamp is not accepted",
		value:   "1714777860",
		wantErr: true,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.value != "" {
				t.Setenv(seedBirthdayEnvVar, test.value)
			}

			got, err := LoadSeedBirthdayFromEnv()
			if test.wantErr {
				require.Error(t, err)

				// A rejected value must not leak a usable
				// birthday to the caller.
				require.True(t, got.IsZero())

				return
			}

			require.NoError(t, err)
			require.True(
				t, test.want.Equal(got),
				"want %v, got %v", test.want, got,
			)
		})
	}
}
