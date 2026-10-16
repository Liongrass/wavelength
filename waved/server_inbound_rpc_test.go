package waved

import (
	"context"
	"testing"

	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"
)

// recordingReplyEdge captures the envelopes handleInboundRPC sends. A set
// sendStatus is returned verbatim, so a test can drive a mailbox rejection
// reported in the response rather than as a transport error.
type recordingReplyEdge struct {
	sent       []*mailboxpb.Envelope
	sendStatus *mailboxpb.Status
}

// Send records the outbound envelope and reports the configured status.
func (r *recordingReplyEdge) Send(_ context.Context, in *mailboxpb.SendRequest,
	_ ...grpc.CallOption) (*mailboxpb.SendResponse, error) {

	r.sent = append(r.sent, in.Envelope)

	status := r.sendStatus
	if status == nil {
		status = &mailboxpb.Status{Ok: true}
	}

	return &mailboxpb.SendResponse{Status: status}, nil
}

// Pull is unused by these tests.
func (r *recordingReplyEdge) Pull(_ context.Context, _ *mailboxpb.PullRequest,
	_ ...grpc.CallOption) (*mailboxpb.PullResponse, error) {

	return &mailboxpb.PullResponse{Status: &mailboxpb.Status{Ok: true}}, nil
}

// AckUpTo is unused by these tests.
func (r *recordingReplyEdge) AckUpTo(_ context.Context,
	_ *mailboxpb.AckUpToRequest, _ ...grpc.CallOption) (
	*mailboxpb.AckUpToResponse, error) {

	return &mailboxpb.AckUpToResponse{
		Status: &mailboxpb.Status{
			Ok: true,
		},
	}, nil
}

// newInboundRPCTestServer builds a server that answers inbound requests on
// edge, with an empty mux so every request takes the error-response path.
func newInboundRPCTestServer(t *testing.T, edge *recordingReplyEdge) *Server {
	t.Helper()

	s := newCompatTestServer(t, edge)
	s.mailboxMux = mailboxrpc.NewServeMux()
	s.localMailboxID = "this-daemon"
	s.remoteMailboxID = "operator:this-daemon"

	return s
}

// inboundRequest builds an inbound request from sender naming replyTo.
func inboundRequest(sender, replyTo string) *mailboxpb.Envelope {
	return &mailboxpb.Envelope{
		Sender: sender,
		Body:   &anypb.Any{},
		Rpc: &mailboxpb.RpcMeta{
			Kind:          mailboxpb.RpcMeta_KIND_REQUEST,
			Service:       "svc.Unregistered",
			Method:        "Method",
			CorrelationId: "corr-1",
			ReplyTo:       replyTo,
		},
	}
}

// TestHandleInboundRPCAnswersTheOperator verifies that an inbound request is
// answered to the operator's mailbox whatever its Sender and ReplyTo name.
func TestHandleInboundRPCAnswersTheOperator(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		sender  string
		replyTo string
	}{
		{
			name:    "operator request",
			sender:  "operator:this-daemon",
			replyTo: "operator:this-daemon",
		},
		{
			name:    "absent reply-to",
			sender:  "operator:this-daemon",
			replyTo: "",
		},
		{
			name:    "reply-to naming another mailbox",
			sender:  "operator:this-daemon",
			replyTo: "attacker",
		},
		{
			name:    "foreign sender",
			sender:  "attacker",
			replyTo: "attacker",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			edge := &recordingReplyEdge{}
			s := newInboundRPCTestServer(t, edge)

			err := s.handleInboundRPC(
				t.Context(), edge,
				inboundRequest(tc.sender, tc.replyTo),
			)
			require.NoError(t, err)

			require.Len(t, edge.sent, 1)
			require.Equal(
				t, "operator:this-daemon",
				edge.sent[0].Recipient,
			)
		})
	}
}

// TestHandleInboundRPCSurfacesSendRejection verifies that a response the
// mailbox rejects is reported as an error rather than as a delivered answer.
func TestHandleInboundRPCSurfacesSendRejection(t *testing.T) {
	t.Parallel()

	edge := &recordingReplyEdge{
		sendStatus: &mailboxpb.Status{
			Ok:      false,
			Code:    "resource_exhausted",
			Message: "inbox full",
		},
	}
	s := newInboundRPCTestServer(t, edge)

	err := s.handleInboundRPC(
		t.Context(), edge,
		inboundRequest("operator:this-daemon", "operator:this-daemon"),
	)
	require.ErrorContains(t, err, "inbox full")
}
