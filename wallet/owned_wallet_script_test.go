package wallet

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// stubOwnedScripts is a scripted OwnedWalletScriptChecker.
type stubOwnedScripts struct {
	owned bool
	err   error
}

// IsOwnedWalletScript returns the scripted answer.
func (s stubOwnedScripts) IsOwnedWalletScript(context.Context, []byte) (bool,
	error) {

	return s.owned, s.err
}

// TestDestinationIsOwnWalletFailsClosed proves every way of not knowing
// resolves to "not ours". A leave is a funds-moving operation, so it must not
// fail because an accounting flag could not be resolved, and overstating
// ownership would understate what the client paid out.
func TestDestinationIsOwnWalletFailsClosed(t *testing.T) {
	t.Parallel()

	script := []byte{0x51, 0x20, 0x01}

	tests := []struct {
		name    string
		checker OwnedWalletScriptChecker
		script  []byte
		want    bool
	}{{
		name:   "registry not wired",
		script: script,
	}, {
		name: "empty script",
		checker: stubOwnedScripts{
			owned: true,
		},
		script: nil,
	}, {
		name: "lookup error",
		checker: stubOwnedScripts{
			err: fmt.Errorf("db down"),
		},
		script: script,
	}, {
		name: "script unknown to the registry",
		checker: stubOwnedScripts{
			owned: false,
		},
		script: script,
	}, {
		name: "script minted by this daemon",
		checker: stubOwnedScripts{
			owned: true,
		},
		script: script,
		want:   true,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &Ark{ownedScripts: tc.checker}
			require.Equal(
				t, tc.want,
				a.destinationIsOwnWallet(
					t.Context(), tc.script,
				),
			)
		})
	}
}
