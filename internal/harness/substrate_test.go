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
	"fmt"
	"io"
	"net"
	"sync"
	"testing"

	"github.com/agent-substrate/substrate/proto/ateapipb"
	"github.com/google/ax/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type mockControlServer struct {
	ateapipb.UnimplementedControlServer
	mu           sync.Mutex
	createdID    string
	suspendedID  string
	createCount  int
	suspendCount int
	workerIP     string
}

func (m *mockControlServer) CreateActor(ctx context.Context, req *ateapipb.CreateActorRequest) (*ateapipb.CreateActorResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createdID = req.ActorId
	m.createCount++
	return &ateapipb.CreateActorResponse{
		Actor: &ateapipb.Actor{
			ActorId:    req.ActorId,
			AteomPodIp: m.workerIP,
		},
	}, nil
}

func (m *mockControlServer) SuspendActor(ctx context.Context, req *ateapipb.SuspendActorRequest) (*ateapipb.SuspendActorResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.suspendedID = req.ActorId
	m.suspendCount++
	return &ateapipb.SuspendActorResponse{}, nil
}

type mockHarnessServiceServer struct {
	proto.UnimplementedHarnessServiceServer
	mu           sync.Mutex
	receivedMsgs []*proto.Message
}

func (s *mockHarnessServiceServer) Connect(stream grpc.BidiStreamingServer[proto.HarnessMessage, proto.HarnessMessage]) error {
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.receivedMsgs = append(s.receivedMsgs, req.Messages...)
		s.mu.Unlock()

		// Send back a mock reply
		resp := &proto.HarnessMessage{
			Messages: []*proto.Message{
				{
					Role: "assistant",
					Content: &proto.Content{
						Type: &proto.Content_Text{
							Text: &proto.TextContent{
								Text: "mock-response",
							},
						},
					},
				},
			},
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

type testHandler struct {
	mu           sync.Mutex
	messages     []*proto.Message
	completeExec string
}

func (t *testHandler) OnMessage(ctx context.Context, execID string, msg *proto.Message) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.messages = append(t.messages, msg)
	return nil
}

func (t *testHandler) OnComplete(ctx context.Context, execID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.completeExec = execID
	return nil
}

func TestSubstrateHarnessDirect(t *testing.T) {
	// 1. Setup local gRPC Control Server & local gRPC Harness Service Server
	lisControl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lisControl.Close()

	lisHarness, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lisHarness.Close()

	// Get ports/IPs
	harnessIP, harnessPortStr, err := net.SplitHostPort(lisHarness.Addr().String())
	if err != nil {
		t.Fatalf("failed to split host/port: %v", err)
	}
	var harnessPort int
	_, err = fmt.Sscanf(harnessPortStr, "%d", &harnessPort)
	if err != nil {
		t.Fatalf("failed to parse port: %v", err)
	}

	srvControl := grpc.NewServer()
	mockControl := &mockControlServer{workerIP: harnessIP}
	ateapipb.RegisterControlServer(srvControl, mockControl)

	srvHarness := grpc.NewServer()
	mockHarness := &mockHarnessServiceServer{}
	proto.RegisterHarnessServiceServer(srvHarness, mockHarness)

	go func() {
		if err := srvControl.Serve(lisControl); err != nil {
			// ignore
		}
	}()
	defer srvControl.Stop()

	go func() {
		if err := srvHarness.Serve(lisHarness); err != nil {
			// ignore
		}
	}()
	defer srvHarness.Stop()

	// 2. Create SubstrateHarness
	subHarness, err := NewSubstrateHarness(SubstrateHarnessConfig{
		SubstrateAPIEndpoint: lisControl.Addr().String(),
		HarnessNamespace:     "test-ns",
		HarnessTemplate:      "test-template",
		Port:                 harnessPort,
		DialOpts:             []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())},
	})
	if err != nil {
		t.Fatalf("failed to create SubstrateHarness: %v", err)
	}
	defer subHarness.ateClient.Close()

	// 4. Start execution
	ctx := context.Background()
	exec, err := subHarness.Start(ctx, "conv-123")
	if err != nil {
		t.Fatalf("failed to start SubstrateHarness: %v", err)
	}

	// Verify CreateActor was called
	mockControl.mu.Lock()
	if mockControl.createCount != 1 {
		t.Errorf("expected CreateActor to be called 1 time, got %d", mockControl.createCount)
	}
	if mockControl.createdID != "conv-123" {
		t.Errorf("expected CreateActor with id conv-123, got %s", mockControl.createdID)
	}
	mockControl.mu.Unlock()

	// 5. Queue input and Run execution
	inputMsg := &proto.Message{
		Role: "user",
		Content: &proto.Content{
			Type: &proto.Content_Text{
				Text: &proto.TextContent{
					Text: "hello",
				},
			},
		},
	}
	if err := exec.Queue(ctx, inputMsg); err != nil {
		t.Fatalf("failed to queue message: %v", err)
	}

	handler := &testHandler{}
	if err := exec.Run(ctx, handler); err != nil {
		t.Fatalf("failed to run execution: %v", err)
	}

	// Verify the mock server received the queued message
	mockHarness.mu.Lock()
	if len(mockHarness.receivedMsgs) != 1 {
		t.Errorf("expected mock HarnessService to receive 1 message, got %d", len(mockHarness.receivedMsgs))
	} else if mockHarness.receivedMsgs[0].GetContent().GetText().GetText() != "hello" {
		t.Errorf("expected received message 'hello', got '%s'", mockHarness.receivedMsgs[0].GetContent().GetText().GetText())
	}
	mockHarness.mu.Unlock()

	// Verify the handler received the response message
	handler.mu.Lock()
	if len(handler.messages) != 1 {
		t.Errorf("expected handler to receive 1 message, got %d", len(handler.messages))
	} else if handler.messages[0].GetContent().GetText().GetText() != "mock-response" {
		t.Errorf("expected handler message 'mock-response', got '%s'", handler.messages[0].GetContent().GetText().GetText())
	}
	if handler.completeExec != exec.ID() {
		t.Errorf("expected OnComplete with exec ID %s, got %s", exec.ID(), handler.completeExec)
	}
	handler.mu.Unlock()

	// 6. Close execution and verify SuspendActor was called
	err = exec.Close(ctx)
	if err != nil {
		t.Fatalf("failed to close execution: %v", err)
	}

	mockControl.mu.Lock()
	if mockControl.suspendCount != 1 {
		t.Errorf("expected SuspendActor to be called 1 time, got %d", mockControl.suspendCount)
	}
	if mockControl.suspendedID != "conv-123" {
		t.Errorf("expected SuspendActor with id conv-123, got %s", mockControl.suspendedID)
	}
	mockControl.mu.Unlock()
}
