//go:build systest

package systest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/wavelength/harness"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/connectivity"
)

// TestLNDSeedRestore proves that a deleted wallet recovers default-account
// coins from its seed, while custom-account scanning requires both branches
// to be reconstructed first. The custom fixture imports an xpub because the
// pinned LND image predates native custom-account creation. It intentionally
// remains watch-only; spendability is proved separately for the default
// account, which has private keys from the restored seed.
func TestLNDSeedRestore(t *testing.T) {
	ParallelN(t)

	opts := harness.DefaultOptions()
	opts.GroupName = t.Name()
	h := harness.NewHarness(t, &opts)
	t.Cleanup(h.Stop)
	h.Start()

	inst, mnemonic := h.StartAdditionalLNDWithSeed("seed-recovery")
	require.Len(t, mnemonic, 24)
	require.Same(t, inst, h.GetAdditionalLND(inst.Name))

	// Import a different node's account xpub to exercise custom-account
	// rescan behavior with the stock harness image and public RPCs.
	accounts, err := h.LND.WalletKit.ListAccounts(
		t.Context(),
		"default", walletrpc.AddressType_WITNESS_PUBKEY_HASH,
	)
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	const custom = "recovery-custom"
	xpub := accounts[0].ExtendedPublicKey
	importSeedRestoreAccount(t, inst, custom, xpub)

	// Fund non-first addresses so an off-by-one count or deriving only
	// address zero cannot pass. Include both receive and change branches.
	for _, change := range []bool{false, true} {
		addr, err := inst.Client.WalletKit.NextAddr(
			t.Context(),
			"default", walletrpc.AddressType_WITNESS_PUBKEY_HASH,
			change,
		)
		require.NoError(t, err)
		h.Faucet(addr.String(), btcutil.Amount(100_000))

		for range 3 {
			addr, err = inst.Client.WalletKit.NextAddr(
				t.Context(), custom,
				walletrpc.AddressType_WITNESS_PUBKEY_HASH,
				change,
			)
			require.NoError(t, err)
		}
		h.Faucet(addr.String(), btcutil.Amount(200_000))
	}
	h.GenerateAndWait(1)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		for _, account := range []string{custom, "default"} {
			coins, err := inst.Client.WalletKit.ListUnspent(
				t.Context(), 1, 100000,
				lndclient.WithUnspentAccount(account),
			)
			require.NoError(c, err)
			require.Len(c, coins, 2)
		}
	}, 30*time.Second, 100*time.Millisecond)

	before := seedRestoreCoins(t, inst, custom)
	defaultBefore := seedRestoreCoins(t, inst, "default")
	recorded := seedRestoreAccount(t, inst, custom)
	require.NotNil(t, recorded)
	require.EqualValues(t, 3, recorded.ExternalKeyCount)
	require.EqualValues(t, 3, recorded.InternalKeyCount)

	// A marker in the wallet directory must disappear. TLS outside it
	// survives, and the old connection must not keep reconnecting.
	marker := filepath.Join(filepath.Dir(inst.Macaroon), "restore-marker")
	require.NoError(t, os.WriteFile(marker, []byte("old wallet"), 0o600))
	cert, err := os.ReadFile(inst.TLSCert)
	require.NoError(t, err)
	oldConn := inst.Client.ClientConn
	h.RestoreLNDFromSeed(inst, mnemonic)
	require.Equal(t, connectivity.Shutdown, oldConn.GetState())
	require.NoFileExists(t, marker)
	restoredCert, err := os.ReadFile(inst.TLSCert)
	require.NoError(t, err)
	require.Equal(t, cert, restoredCert)
	require.Same(t, inst, h.GetAdditionalLND(inst.Name))
	require.Nil(t, seedRestoreAccount(t, inst, custom))
	require.Equal(t, defaultBefore, seedRestoreCoins(t, inst, "default"))

	// Account identity and issued addresses are separate prerequisites.
	// Reimporting only the xpub must not rediscover its funded addresses.
	importSeedRestoreAccount(t, inst, custom, xpub)
	rebuilt := seedRestoreAccount(t, inst, custom)
	require.NotNil(t, rebuilt)
	require.Equal(t, recorded.DerivationPath, rebuilt.DerivationPath)
	require.Equal(t, recorded.ExtendedPublicKey, rebuilt.ExtendedPublicKey)
	h.RescanLND(inst)
	require.Empty(t, seedRestoreCoins(t, inst, custom))

	// Reconstructing only receive addresses recovers exactly one output.
	// A successful rescan is therefore not proof of complete recovery.
	deriveSeedRestoreBranch(
		t, inst, custom, recorded.ExternalKeyCount, false,
	)
	h.RescanLND(inst)
	partial := seedRestoreCoins(t, inst, custom)
	require.Len(t, partial, 1)
	for outpoint, amount := range partial {
		require.Equal(t, before[outpoint], amount)
	}

	deriveSeedRestoreBranch(
		t, inst, custom, recorded.InternalKeyCount, true,
	)
	oldConn = inst.Client.ClientConn
	h.RescanLND(inst)
	require.Equal(t, connectivity.Shutdown, oldConn.GetState())
	require.Equal(t, before, seedRestoreCoins(t, inst, custom))
	require.Equal(t, defaultBefore, seedRestoreCoins(t, inst, "default"))

	// Spend every restored default-account output. Exact input membership
	// proves the signature came from recovered keys for both branches.
	dest, err := h.LND.WalletKit.NextAddr(
		t.Context(),
		"default", walletrpc.AddressType_WITNESS_PUBKEY_HASH, false,
	)
	require.NoError(t, err)
	txid, err := inst.Client.Client.SendCoins(
		t.Context(), dest, 0, true, 0, 2, "seed recovery proof",
	)
	require.NoError(t, err)
	client, err := h.BitcoinRPCClient()
	require.NoError(t, err)
	defer client.Shutdown()
	hash, err := chainhash.NewHashFromStr(txid)
	require.NoError(t, err)
	tx, err := client.GetRawTransaction(hash)
	require.NoError(t, err)
	require.Len(t, tx.MsgTx().TxIn, len(defaultBefore))
	for _, input := range tx.MsgTx().TxIn {
		require.Contains(
			t, defaultBefore, input.PreviousOutPoint.String(),
		)
	}
	blocks := h.GenerateAndWait(1)
	require.Contains(t, blocks[0].TxIDs, txid)
}

// importSeedRestoreAccount recreates a watch-only custom account from its
// recorded xpub using RPCs available in the pinned harness LND image.
func importSeedRestoreAccount(t *testing.T, inst *harness.LndInstance, name,
	xpub string) {

	t.Helper()
	ctx, timeout, client := inst.Client.WalletKit.RawClientWithMacAuth(
		t.Context(),
	)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err := client.ImportAccount(ctx, &walletrpc.ImportAccountRequest{
		Name: name, ExtendedPublicKey: xpub,
		AddressType: walletrpc.AddressType_WITNESS_PUBKEY_HASH,
	})
	require.NoError(t, err)
}

// seedRestoreAccount distinguishes authoritative absence from an RPC error.
func seedRestoreAccount(t *testing.T, inst *harness.LndInstance,
	name string) *walletrpc.Account {

	t.Helper()
	accounts, err := inst.Client.WalletKit.ListAccounts(
		t.Context(), "", walletrpc.AddressType_WITNESS_PUBKEY_HASH,
	)
	require.NoError(t, err)
	for _, account := range accounts {
		if account.Name == name {
			return account
		}
	}

	return nil
}

// seedRestoreCoins snapshots exact confirmed outpoints and amounts so equal
// balances cannot hide missing or substituted recovered outputs.
func seedRestoreCoins(t *testing.T, inst *harness.LndInstance,
	account string) map[string]btcutil.Amount {

	t.Helper()
	coins, err := inst.Client.WalletKit.ListUnspent(
		t.Context(), 1, 100000, lndclient.WithUnspentAccount(account),
	)
	require.NoError(t, err)
	result := make(map[string]btcutil.Amount, len(coins))
	for _, coin := range coins {
		result[coin.OutPoint.String()] = coin.Value
	}

	return result
}

// deriveSeedRestoreBranch restores all recorded addresses on one branch,
// including the final issued address where this test placed its funds.
func deriveSeedRestoreBranch(t *testing.T, inst *harness.LndInstance,
	account string, count uint32, change bool) {

	t.Helper()
	for range count {
		_, err := inst.Client.WalletKit.NextAddr(
			t.Context(), account,
			walletrpc.AddressType_WITNESS_PUBKEY_HASH, change,
		)
		require.NoError(t, err)
	}
}
