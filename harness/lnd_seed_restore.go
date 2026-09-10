package harness

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

const (
	// seedRestoreRecoveryWindow is the address look-ahead lnd uses when a
	// wallet is restored from a seed.
	//
	// It only governs lnd's own scan of the default account. A custom
	// account is invisible to that scan whatever the window, which is the
	// reason a restore test exists at all — but the window still has to be
	// non-zero to enable default-account address discovery.
	seedRestoreRecoveryWindow = 250

	// walletUnlockerTimeout bounds a single wallet unlocker RPC.
	walletUnlockerTimeout = 30 * time.Second
)

// lndWalletPassword is the password every harness-managed lnd wallet uses.
// It has to be stable across a restore, since the wallet is re-initialized
// with it after the original database is destroyed.
var lndWalletPassword = []byte("itestpassword")

// StartAdditionalLNDWithSeed launches an extra lnd whose wallet is created
// from a freshly generated aezeed, and returns the instance together with that
// seed's mnemonic. Serialize calls with other harness lifecycle operations.
//
// The harness normally starts lnd with --noseedbackup, which auto-creates a
// wallet whose seed is never surfaced. That is fine until a test needs to
// destroy the wallet and bring it back, which is impossible without the
// mnemonic — so this path initializes the wallet explicitly instead.
func (h *Harness) StartAdditionalLNDWithSeed(name string) (*LndInstance,
	[]string) {

	h.T.Helper()

	if name == "" {
		name = fmt.Sprintf("lnd-seeded-%d", len(h.extraLNDs)+1)
	}
	if _, exists := h.extraLNDs[name]; exists {
		h.T.Fatalf("LND instance %s already exists", name)
	}

	dataDir := filepath.Join(h.artifactsDir, name)
	inst := h.startLNDInstanceWithOptions(
		name, dataDir, false, LNDChainBackendBitcoind, true, nil,
	)

	// Register before initialization so Stop also cleans up a failed init.
	h.extraLNDs[name] = inst
	h.T.Cleanup(func() {
		if inst.Client != nil {
			_ = inst.Client.ClientConn.Close()
		}
	})
	mnemonic := h.initLNDFromSeed(inst, nil)
	h.initAndWaitLNDInstance(inst)

	return inst, mnemonic
}

// RestoreLNDFromSeed destroys an instance's wallet and brings it back from the
// given mnemonic.
//
// This is the failure the recovery procedure exists for: the seed survives and
// nothing else does. The wallet database is deleted rather than merely reset
// so that the restored node genuinely rediscovers its state from the chain.
//
// LND performs its normal default-account seed recovery and chain sync.
// Custom accounts require the caller to reconstruct the original key scope
// and account index, verify the account xpub, and re-derive at least the
// original external AND internal address counts. Call RescanLND afterwards;
// a synchronized wallet alone does not prove that all funds were recovered.
//
// The container is recreated, so its ports may change; the instance is
// the same pointer with its fields refreshed, and any client built against the
// old ports has to be rebuilt. The old harness RPC connection is closed.
// Use only instances returned by StartAdditionalLNDWithSeed, with callers
// quiesced. Lifecycle calls and use of inst must be serialized with Stop.
func (h *Harness) RestoreLNDFromSeed(inst *LndInstance, mnemonic []string) {
	h.T.Helper()

	require.NotNil(h.T, inst, "instance is required")
	require.Len(h.T, mnemonic, 24, "aezeed mnemonic must be 24 words")

	h.Logf("Destroying %s wallet and restoring from seed...", inst.Name)

	h.stopSeedLND(inst)

	// Remove the chain-specific state, which is where the wallet database
	// and the macaroons live. Everything under it is regenerated on
	// re-init; the TLS material above it is deliberately kept so clients
	// keep trusting the same certificate.
	chainDir := filepath.Join(
		inst.DataDir, "data", "chain", "bitcoin", "regtest",
	)
	require.NoError(
		h.T, os.RemoveAll(chainDir),
		"remove %s wallet state", inst.Name,
	)

	res := h.startLNDContainer(lndConfig{
		name:             inst.Name,
		dataDir:          inst.DataDir,
		bitcoindName:     h.bitcoindName,
		network:          h.network,
		group:            h.group,
		image:            imageRepo(h.opts.LNDImage),
		tag:              imageTag(h.opts.LNDImage),
		manualWalletInit: true,
		chainBackend:     LNDChainBackendBitcoind,
	})

	inst.Resource = res
	inst.GRPCPort = res.GetPort("10009/tcp")
	inst.RESTPort = res.GetPort("8080/tcp")
	inst.ContainerName = strings.TrimPrefix(res.Container.Name, "/")
	inst.Client = nil

	h.waitForLNDDial(inst)
	h.initLNDFromSeed(inst, mnemonic)
	h.initAndWaitLNDInstance(inst)

	h.Logf("%s restored from seed", inst.Name)
}

// initLNDFromSeed initializes an instance's wallet, generating a fresh aezeed
// when no mnemonic is supplied and restoring the given one otherwise. It
// returns the mnemonic the wallet was created from.
func (h *Harness) initLNDFromSeed(inst *LndInstance,
	mnemonic []string) []string {

	h.T.Helper()

	tlsCert, err := loadClientTLSCredentials(inst.TLSCert)
	require.NoError(h.T, err, "load %s TLS credentials", inst.Name)

	addr := net.JoinHostPort("127.0.0.1", inst.GRPCPort)
	conn, err := grpc.NewClient(
		addr, grpc.WithTransportCredentials(tlsCert),
	)
	require.NoError(h.T, err, "connect to %s wallet unlocker", inst.Name)
	defer conn.Close()

	h.waitForWalletState(conn, inst, lnrpc.WalletState_NON_EXISTING)

	unlocker := lnrpc.NewWalletUnlockerClient(conn)

	// A restore reuses the caller's mnemonic; a fresh start asks lnd for
	// one so the test has something to restore from later.
	recoveryWindow := int32(seedRestoreRecoveryWindow)
	if len(mnemonic) == 0 {
		ctx, cancel := context.WithTimeout(
			h.T.Context(), walletUnlockerTimeout,
		)
		defer cancel()

		seed, err := unlocker.GenSeed(ctx, &lnrpc.GenSeedRequest{})
		require.NoError(h.T, err, "generate %s seed", inst.Name)

		mnemonic = seed.GetCipherSeedMnemonic()

		// A brand new wallet has nothing to recover, and asking for a
		// recovery on one makes lnd scan the chain for no reason.
		recoveryWindow = 0
	}

	ctx, cancel := context.WithTimeout(
		h.T.Context(), walletUnlockerTimeout,
	)
	defer cancel()

	_, err = unlocker.InitWallet(ctx, &lnrpc.InitWalletRequest{
		WalletPassword:     lndWalletPassword,
		CipherSeedMnemonic: mnemonic,
		RecoveryWindow:     recoveryWindow,
	})
	require.NoError(h.T, err, "initialize %s wallet", inst.Name)

	return mnemonic
}

// waitForWalletState blocks until an instance's wallet reports the given state.
func (h *Harness) waitForWalletState(conn *grpc.ClientConn, inst *LndInstance,
	want lnrpc.WalletState) {

	h.T.Helper()

	stateClient := lnrpc.NewStateClient(conn)
	require.Eventually(
		h.T, func() bool {
			const checkTimeout = 5 * time.Second

			ctx, cancel := context.WithTimeout(
				h.T.Context(), checkTimeout,
			)
			defer cancel()

			resp, err := stateClient.GetState(
				ctx, &lnrpc.GetStateRequest{},
			)
			if err != nil {
				return false
			}

			return resp.State == want
		},
		lndStartupTimeout, time.Second,
		fmt.Sprintf("%s wallet did not reach %v", inst.Name, want),
	)
}

// waitForLNDDial blocks until an instance serves TLS and accepts a connection.
func (h *Harness) waitForLNDDial(inst *LndInstance) {
	h.T.Helper()

	require.Eventually(
		h.T, func() bool {
			if !lndTLSReady(inst.TLSCert) {
				return false
			}

			addr := net.JoinHostPort("127.0.0.1", inst.GRPCPort)
			conn, err := net.DialTimeout("tcp", addr, time.Second)
			if err != nil {
				return false
			}
			_ = conn.Close()

			return true
		},
		lndStartupTimeout, time.Second,
		fmt.Sprintf("%s TLS/gRPC not ready", inst.Name),
	)
}

// RescanLND restarts an instance with --reset-wallet-transactions, forcing it
// to rescan the chain for every address its wallet currently knows.
//
// This is the last step of recovering a custom account, and the order matters:
// a rescan searches only for addresses already present in the wallet database,
// so running it before the account is recreated and its addresses re-derived
// finds nothing and reports an empty balance for a funded account.
//
// The wallet already exists here, so the node comes back locked and is
// unlocked rather than initialized. As with a restore, the container is
// recreated and the instance's ports may change. The old harness RPC
// connection is closed. Use only instances from StartAdditionalLNDWithSeed
// and serialize lifecycle calls and use of inst with Stop. Returning means
// chain sync is complete, not that the caller rebuilt every funded address.
func (h *Harness) RescanLND(inst *LndInstance) {
	h.T.Helper()

	require.NotNil(h.T, inst, "instance is required")

	h.Logf("Rescanning %s from genesis...", inst.Name)

	h.stopSeedLND(inst)

	res := h.startLNDContainer(lndConfig{
		name:             inst.Name,
		dataDir:          inst.DataDir,
		bitcoindName:     h.bitcoindName,
		network:          h.network,
		group:            h.group,
		image:            imageRepo(h.opts.LNDImage),
		tag:              imageTag(h.opts.LNDImage),
		manualWalletInit: true,
		chainBackend:     LNDChainBackendBitcoind,
		extraArgs:        []string{"--reset-wallet-transactions"},
	})

	inst.Resource = res
	inst.GRPCPort = res.GetPort("10009/tcp")
	inst.RESTPort = res.GetPort("8080/tcp")
	inst.ContainerName = strings.TrimPrefix(res.Container.Name, "/")
	inst.Client = nil

	h.waitForLNDDial(inst)
	h.unlockLND(inst)
	h.initAndWaitLNDInstance(inst)

	h.Logf("%s rescan complete", inst.Name)
}

// unlockLND unlocks an instance whose wallet already exists.
func (h *Harness) unlockLND(inst *LndInstance) {
	h.T.Helper()

	tlsCert, err := loadClientTLSCredentials(inst.TLSCert)
	require.NoError(h.T, err, "load %s TLS credentials", inst.Name)

	addr := net.JoinHostPort("127.0.0.1", inst.GRPCPort)
	conn, err := grpc.NewClient(
		addr, grpc.WithTransportCredentials(tlsCert),
	)
	require.NoError(h.T, err, "connect to %s wallet unlocker", inst.Name)
	defer conn.Close()

	h.waitForWalletState(conn, inst, lnrpc.WalletState_LOCKED)

	ctx, cancel := context.WithTimeout(
		h.T.Context(), walletUnlockerTimeout,
	)
	defer cancel()

	_, err = lnrpc.NewWalletUnlockerClient(conn).UnlockWallet(
		ctx, &lnrpc.UnlockWalletRequest{
			WalletPassword: lndWalletPassword,
		},
	)
	require.NoError(h.T, err, "unlock %s wallet", inst.Name)
}

// stopSeedLND closes the old RPC connection and requires removal of the exact
// container before its wallet files can be deleted or reopened by a restart.
// Best-effort name cleanup is insufficient at this destructive boundary.
func (h *Harness) stopSeedLND(inst *LndInstance) {
	h.T.Helper()

	if inst.Client != nil {
		_ = inst.Client.ClientConn.Close()
		inst.Client = nil
	}

	require.NoError(
		h.T, h.pool.Purge(inst.Resource),
		"remove %s before changing wallet state", inst.Name,
	)
}
