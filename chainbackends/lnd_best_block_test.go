package chainbackends

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/stretchr/testify/require"
)

// liveTipChainKit supplies live chain reads independently of block events.
type liveTipChainKit struct {
	lndclient.ChainKitClient

	height int32
	hash   chainhash.Hash
	err    error
}

// GetBestBlock returns the configured tip or query failure.
func (c *liveTipChainKit) GetBestBlock(ctx context.Context) (chainhash.Hash,
	int32, error) {

	if err := ctx.Err(); err != nil {
		return chainhash.Hash{}, 0, err
	}

	return c.hash, c.height, c.err
}

// TestRemoteBestBlockReadsLiveTip verifies constructor wiring and fresh reads
// without registering a block stream. The unused notifier panics if accessed.
func TestRemoteBestBlockReadsLiveTip(t *testing.T) {
	t.Parallel()

	chainKit := &liveTipChainKit{
		height: 288,
		hash: chainhash.Hash{
			2,
		},
	}
	backend, err := NewLNDBackendFromLndClient(
		LNDBackendFromLndClientConfig{
			LND: &lndclient.LndServices{
				ChainKit:      chainKit,
				ChainNotifier: &stubLndClientNotifier{},
				WalletKit:     &stubWalletKitFeeEstimator{},
			},
		},
	)
	require.NoError(t, err)

	height, hash, err := backend.BestBlock(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 288, height)
	require.Equal(t, chainKit.hash, hash)

	// A reorg lowers the next read rather than preserving a cached height.
	chainKit.height = 287
	chainKit.hash = chainhash.Hash{3}
	height, hash, err = backend.BestBlock(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 287, height)
	require.Equal(t, chainKit.hash, hash)
}

// TestRemoteBestBlockPropagatesErrors verifies that RPC failures and caller
// cancellation return errors without querying the notification tip.
func TestRemoteBestBlockPropagatesErrors(t *testing.T) {
	t.Parallel()

	rpcErr := errors.New("chain query unavailable")
	for _, wantErr := range []error{rpcErr, context.Canceled} {
		t.Run(wantErr.Error(), func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			if errors.Is(wantErr, context.Canceled) {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			services := &lndclient.LndServices{
				ChainKit: &liveTipChainKit{
					err: wantErr,
				},
				ChainNotifier: &stubLndClientNotifier{},
				WalletKit:     &stubWalletKitFeeEstimator{},
			}
			backend, err := NewLNDBackendFromLndClient(
				LNDBackendFromLndClientConfig{
					LND: services,
				},
			)
			require.NoError(t, err)

			height, hash, err := backend.BestBlock(ctx)
			require.ErrorIs(t, err, wantErr)
			require.Zero(t, height)
			require.Equal(t, chainhash.Hash{}, hash)
		})
	}
}
