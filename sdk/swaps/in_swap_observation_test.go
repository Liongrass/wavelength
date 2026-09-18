package swaps

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestPayWaitKeepsPollingInconclusiveFunding proves a pending or unavailable
// live observation cannot abort claim monitoring or authorize a refund. The
// context ends the wait after repeated observations, without a terminal state.
func TestPayWaitKeepsPollingInconclusiveFunding(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	daemon := &testDaemonConn{
		liveLookupHook: func(call int) (*VTXOInfo, error) {
			if call == 3 {
				cancel()
			}

			return nil, errors.New("pending funding observation")
		},
	}
	client := NewSwapClient(nil, daemon, nil, nil)
	client.waitPollInterval = time.Millisecond
	session := &paySession{
		client: client,
		state:  PayStateWaitingForClaim,
		cfg: &InSwapConfig{
			Expiry: time.Now().Add(time.Hour),
			VHTLCConfig: VHTLCConfig{
				RefundLocktime: 1000,
			},
		},
	}
	err := session.waitForClaimPreimage(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 3, daemon.liveLookupCalls)
	require.Equal(t, PayStateWaitingForClaim, session.State())
	require.Zero(t, daemon.sendCustomCalls)
	require.Zero(t, daemon.sendPolicyCalls)
	require.Nil(t, session.preimage)
}
