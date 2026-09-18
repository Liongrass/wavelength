package waved

import (
	"context"
	"time"

	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// policyReceiveRetention is shorter than the operator's 30-day retention cap.
// Funded outputs remain queryable through their persisted policy after this
// binding expires; callers must renew while relying on negative observations.
const policyReceiveRetention = 28 * 24 * time.Hour

// RegisterPolicyReceiveScript registers an exact custom output using the
// daemon identity's policy-participant proof. The operator durably upserts the
// principal/script pair, so retry and daemon restart cannot allocate duplicate
// registrations. Registration never authorizes a spend or funds an output.
func (r *RPCServer) RegisterPolicyReceiveScript(ctx context.Context,
	req *waverpc.RegisterPolicyReceiveScriptRequest) (
	*waverpc.RegisterPolicyReceiveScriptResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}
	if req == nil || len(req.PkScript) == 0 {
		return nil, status.Error(
			codes.InvalidArgument, "missing pk_script",
		)
	}
	policy, err := arkscript.DecodePolicyTemplate(req.PolicyTemplate)
	if err != nil || !policy.MatchesPkScript(req.PkScript) {
		return nil, status.Error(
			codes.InvalidArgument,
			"policy does not match pk_script",
		)
	}
	if r.server.indexer == nil {
		return nil, status.Error(
			codes.Unavailable, "indexer not initialized",
		)
	}
	signerFactory, err := r.server.indexerProofSignerFactory()
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	key := r.server.loadClientKeyDesc()
	if key.PubKey == nil {
		return nil, status.Error(
			codes.Unavailable, "identity key not initialized",
		)
	}

	// The operator verifies membership against its own operator key. Local
	// policy reconstruction catches mismatched output requests before
	// signing.
	client := r.server.indexer.WithSigner(signerFactory(key))
	expiresAt := r.server.clk.Now().Add(policyReceiveRetention)
	_, err = client.RegisterReceiveScriptPolicy(
		ctx, req.PkScript, req.PolicyTemplate, expiresAt,
		"custom policy receive",
	)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "register policy "+
			"receive script: %v", err)
	}

	return &waverpc.RegisterPolicyReceiveScriptResponse{
		ExpiresAtUnixS: uint64(expiresAt.Unix()),
	}, nil
}
