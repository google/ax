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

package agent

import (
	"context"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/google/ax/proto"
)

type mockAgentServiceServer struct {
	proto.UnimplementedAgentServiceServer
	mu       sync.Mutex
	lastReq  *proto.AgentRequest
	response []*proto.AgentResponse
}

func (s *mockAgentServiceServer) Connect(req *proto.AgentRequest, stream grpc.ServerStreamingServer[proto.AgentResponse]) error {
	s.mu.Lock()
	s.lastReq = req
	s.mu.Unlock()

	for _, resp := range s.response {
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

func TestRemoteAgent_Connect(t *testing.T) {
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	srv := grpc.NewServer()
	defer srv.Stop()

	mockServer := &mockAgentServiceServer{
		response: []*proto.AgentResponse{
			{
				ConversationId: "conv-1",
				ExecId:         "exec-1",
				Type: &proto.AgentResponse_Outputs{
					Outputs: &proto.AgentOutputs{
						Messages: []*proto.Message{
							{
								Role: "assistant",
								Content: &proto.Content{
									Type: &proto.Content_Text{
										Text: &proto.TextContent{Text: "Response line 1"},
									},
								},
							},
						},
					},
				},
			},
			{
				ConversationId: "conv-1",
				ExecId:         "exec-1",
				Type: &proto.AgentResponse_End{
					End: &proto.AgentEnd{},
				},
			},
		},
	}
	proto.RegisterAgentServiceServer(srv, mockServer)

	go func() {
		if err := srv.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			t.Errorf("failed to serve: %v", err)
		}
	}()

	addr := lis.Addr().String()
	remoteAgent, err := NewRemoteAgent(RemoteAgentConfig{
		Address: addr,
		DialOpts: []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		},
	})
	if err != nil {
		t.Fatalf("failed to create remote agent: %v", err)
	}
	defer remoteAgent.Close()

	var outputs []*proto.AgentOutputs
	outputHandler := func(outgoing *proto.AgentOutputs) error {
		outputs = append(outputs, outgoing)
		return nil
	}

	start := &proto.AgentStart{
		AgentId: "test-agent",
		Messages: []*proto.Message{
			{
				Role: "user",
				Content: &proto.Content{
					Type: &proto.Content_Text{
						Text: &proto.TextContent{Text: "Request text"},
					},
				},
			},
		},
	}

	err = remoteAgent.Connect(context.Background(), "conv-1", "exec-1", start, nil, outputHandler)
	if err != nil {
		t.Fatalf("RemoteAgent.Connect failed: %v", err)
	}

	mockServer.mu.Lock()
	lastReq := mockServer.lastReq
	mockServer.mu.Unlock()

	if lastReq == nil {
		t.Fatal("server did not receive request")
	}
	if lastReq.ConversationId != "conv-1" {
		t.Errorf("expected conversation ID 'conv-1', got %q", lastReq.ConversationId)
	}
	if lastReq.ExecId != "exec-1" {
		t.Errorf("expected exec ID 'exec-1', got %q", lastReq.ExecId)
	}
	if lastReq.Start.AgentId != "test-agent" {
		t.Errorf("expected agent ID 'test-agent', got %q", lastReq.Start.AgentId)
	}

	if len(outputs) != 1 {
		t.Fatalf("expected exactly 1 output from handler, got %d", len(outputs))
	}
	if len(outputs[0].Messages) != 1 {
		t.Fatalf("expected exactly 1 message in outputs, got %d", len(outputs[0].Messages))
	}
	gotText := outputs[0].Messages[0].GetContent().GetText().GetText()
	if gotText != "Response line 1" {
		t.Errorf("expected response text 'Response line 1', got %q", gotText)
	}
}
