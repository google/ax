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

package harness

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/google/ax/proto"
)

type mockHandler struct {
	mu       sync.Mutex
	messages []*proto.Message
	complete bool
	err      error
}

func (h *mockHandler) OnMessage(ctx context.Context, execID string, msg *proto.Message) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, msg)
	return h.err
}

func (h *mockHandler) OnComplete(ctx context.Context, execID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.complete = true
	return nil
}

// mockHarnessServer implements proto.HarnessServiceServer for testing.
type mockHarnessServer struct {
	proto.UnimplementedHarnessServiceServer
	failConnect bool
}

func (s *mockHarnessServer) Connect(stream proto.HarnessService_ConnectServer) error {
	if s.failConnect {
		return status.Error(codes.Internal, "internal mock server crash")
	}

	// Read the initiating HarnessRequest{start}.
	req, err := stream.Recv()
	if err != nil {
		return err
	}

	// 1. Verify conversation details
	if req.GetConversationId() != "conv-test" {
		return status.Error(codes.InvalidArgument, "invalid conversation_id")
	}

	// 2. Stream thought frame
	tMsg := &proto.Message{
		Role: "model",
		Content: &proto.Content{
			Type: &proto.Content_Thought{
				Thought: &proto.ThoughtContent{
					Summary: []*proto.ThoughtSummaryContent{
						{
							Type: &proto.ThoughtSummaryContent_Text{
								Text: &proto.TextContent{Text: "Analyzing"},
							},
						},
					},
				},
			},
		},
	}
	if err := stream.Send(&proto.HarnessResponse{
		ConversationId: req.GetConversationId(),
		Type: &proto.HarnessResponse_Outputs{
			Outputs: &proto.HarnessOutputs{Messages: []*proto.Message{tMsg}},
		},
	}); err != nil {
		return err
	}

	// 3. Stream text frame
	txtMsg := &proto.Message{
		Role: "assistant",
		Content: &proto.Content{
			Type: &proto.Content_Text{
				Text: &proto.TextContent{Text: "Hello world"},
			},
		},
	}
	if err := stream.Send(&proto.HarnessResponse{
		ConversationId: req.GetConversationId(),
		Type: &proto.HarnessResponse_Outputs{
			Outputs: &proto.HarnessOutputs{Messages: []*proto.Message{txtMsg}},
		},
	}); err != nil {
		return err
	}

	// 4. Stream end frame
	return stream.Send(&proto.HarnessResponse{
		ConversationId: req.GetConversationId(),
		Type: &proto.HarnessResponse_End{
			End: &proto.HarnessEnd{State: proto.State_STATE_COMPLETED},
		},
	})
}

func TestAntigravityHarness_Run_Success(t *testing.T) {
	// Spin up a local TCP listener
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	// Initialize and start local gRPC server
	grpcServer := grpc.NewServer()
	mockServer := &mockHarnessServer{}
	proto.RegisterHarnessServiceServer(grpcServer, mockServer)

	go func() {
		if err := grpcServer.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			t.Errorf("Serve failed: %v", err)
		}
	}()
	defer grpcServer.Stop()

	harnessClient := NewAntigravityHarness(lis.Addr().String())
	exec, err := harnessClient.Start(context.Background(), "conv-test")
	if err != nil {
		t.Fatalf("failed to start execution: %v", err)
	}
	defer exec.Close(context.Background())

	msg := &proto.Message{
		Role: "user",
		Content: &proto.Content{
			Type: &proto.Content_Text{Text: &proto.TextContent{Text: "Hi"}},
		},
	}
	if err := exec.Queue(context.Background(), msg); err != nil {
		t.Fatalf("failed to queue message: %v", err)
	}

	handler := &mockHandler{}
	err = exec.Run(context.Background(), handler)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	handler.mu.Lock()
	defer handler.mu.Unlock()

	if !handler.complete {
		t.Error("expected OnComplete to be called")
	}
	if len(handler.messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(handler.messages))
	}
	if handler.messages[0].GetContent().GetThought().GetSummary()[0].GetText().GetText() != "Analyzing" {
		t.Errorf("expected 'Analyzing', got %q", handler.messages[0].GetContent().GetThought().GetSummary()[0].GetText().GetText())
	}
	if handler.messages[1].GetContent().GetText().GetText() != "Hello world" {
		t.Errorf("expected 'Hello world', got %q", handler.messages[1].GetContent().GetText().GetText())
	}
}

func TestAntigravityHarness_Run_ErrorFrame(t *testing.T) {
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	grpcServer := grpc.NewServer()
	mockServer := &mockHarnessServer{failConnect: true}
	proto.RegisterHarnessServiceServer(grpcServer, mockServer)

	go func() {
		_ = grpcServer.Serve(lis)
	}()
	defer grpcServer.Stop()

	harnessClient := NewAntigravityHarness(lis.Addr().String())
	exec, _ := harnessClient.Start(context.Background(), "conv-test")
	defer exec.Close(context.Background())

	msg := &proto.Message{
		Role: "user",
		Content: &proto.Content{
			Type: &proto.Content_Text{Text: &proto.TextContent{Text: "Hi"}},
		},
	}
	_ = exec.Queue(context.Background(), msg)

	handler := &mockHandler{}
	err = exec.Run(context.Background(), handler)
	if err == nil {
		t.Fatal("expected error from Run(), got nil")
	}
	if !strings.Contains(err.Error(), "internal mock server crash") {
		t.Errorf("unexpected error message: %v", err)
	}
}
