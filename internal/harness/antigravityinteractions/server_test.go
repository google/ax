// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package antigravityinteractions

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"

	"github.com/google/ax/internal/harness/harnesstest"
	"github.com/google/ax/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// startTestServer serves the HarnessService (backed by h) over an in-memory
// bufconn and returns a connected HarnessService client.
func startTestServer(t *testing.T, h *AntigravityInteractionsHarness) proto.HarnessServiceClient {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	s := grpc.NewServer()
	proto.RegisterHarnessServiceServer(s, &server{h: h})
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return proto.NewHarnessServiceClient(conn)
}

// TestConnect_StartToEnd verifies the Connect wire contract: a start frame drives
// one harness turn, and the stream terminates with exactly one HarnessEnd
// (COMPLETED) against the fake Interactions API.
func TestConnect_StartToEnd(t *testing.T) {
	fake := &fakeInteractions{interactionIDs: []string{"INT-1"}}
	h := newTestHarness(t, fake, t.TempDir())
	client := startTestServer(t, h)

	stream, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := stream.Send(&proto.HarnessRequest{
		ConversationId: "conv-1",
		HarnessId:      "antigravity-interactions",
		Type: &proto.HarnessRequest_Start{
			Start: &proto.HarnessStart{Messages: []*proto.Message{harnesstest.UserText("hello")}},
		},
	}); err != nil {
		t.Fatalf("send start: %v", err)
	}
	_ = stream.CloseSend()

	var gotEnd *proto.HarnessEnd
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		if end := resp.GetEnd(); end != nil {
			if gotEnd != nil {
				t.Fatalf("received more than one HarnessEnd")
			}
			gotEnd = end
		}
	}

	if gotEnd == nil {
		t.Fatal("stream ended without a HarnessEnd frame")
	}
	if gotEnd.GetState() != proto.State_STATE_COMPLETED {
		t.Errorf("terminal state = %v, want COMPLETED (error: %v)", gotEnd.GetState(), gotEnd.GetError())
	}

	// The fake recorded the request: first turn has no previous_interaction_id.
	reqs := fake.recorded()
	if len(reqs) != 1 {
		t.Fatalf("fake received %d requests, want 1", len(reqs))
	}
	if reqs[0].PreviousInteractionID != "" {
		t.Errorf("previous_interaction_id = %q, want empty", reqs[0].PreviousInteractionID)
	}
}

// TestConnect_FirstFrameMustBeStart verifies that a non-start first frame is
// rejected.
func TestConnect_FirstFrameMustBeStart(t *testing.T) {
	fake := &fakeInteractions{}
	h := newTestHarness(t, fake, t.TempDir())
	client := startTestServer(t, h)

	stream, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := stream.Send(&proto.HarnessRequest{
		ConversationId: "conv-1",
		Type:           &proto.HarnessRequest_Cancel{Cancel: &proto.HarnessCancel{}},
	}); err != nil {
		t.Fatalf("send cancel: %v", err)
	}
	_ = stream.CloseSend()

	// Draining the stream should surface an error (Connect returns it).
	for {
		_, err := stream.Recv()
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			t.Fatal("expected an error for a non-start first frame, got clean EOF")
		}
		break // got the expected non-EOF error
	}
}
