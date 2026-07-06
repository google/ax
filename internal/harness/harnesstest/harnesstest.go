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

package harnesstest

// Shared in-process mocks for the harness tests: a mock Substrate Control server
// (the substrate control plane), a mock HarnessService server (the harness
// inside an actor), a recording Handler, and message builders.

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/ax/internal/harness"
	"github.com/google/ax/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// mockControlServer is an in-process ateapipb.ControlServer that records the
// actor lifecycle calls SubstrateHarness makes and lets tests steer the
// CreateActor/ResumeActor responses. Only the three RPCs SubstrateHarness uses
// are implemented; the rest come from the embedded Unimplemented server.
type MockControlServer struct {
	ateapipb.UnimplementedControlServer

	mu           sync.Mutex
	createCalls  []string
	resumeCalls  []string
	suspendCalls []string

	CreateErr      error  // returned from CreateActor when non-nil
	ResumeIP       string // AteomPodIp returned from ResumeActor
	ResumeNilActor bool   // when true, ResumeActor returns a nil Actor
}

func (f *MockControlServer) CreateAtespace(_ context.Context, req *ateapipb.CreateAtespaceRequest) (*ateapipb.CreateAtespaceResponse, error) {
	return &ateapipb.CreateAtespaceResponse{Atespace: &ateapipb.Atespace{Name: req.GetName()}}, nil
}

func (f *MockControlServer) CreateActor(_ context.Context, req *ateapipb.CreateActorRequest) (*ateapipb.CreateActorResponse, error) {
	f.mu.Lock()
	f.createCalls = append(f.createCalls, req.GetActorRef().GetName())
	f.mu.Unlock()
	if f.CreateErr != nil {
		return nil, f.CreateErr
	}
	return &ateapipb.CreateActorResponse{Actor: &ateapipb.Actor{ActorId: req.GetActorRef().GetName()}}, nil
}

func (f *MockControlServer) ResumeActor(_ context.Context, req *ateapipb.ResumeActorRequest) (*ateapipb.ResumeActorResponse, error) {
	f.mu.Lock()
	f.resumeCalls = append(f.resumeCalls, req.GetActorRef().GetName())
	f.mu.Unlock()
	if f.ResumeNilActor {
		return &ateapipb.ResumeActorResponse{}, nil
	}
	return &ateapipb.ResumeActorResponse{Actor: &ateapipb.Actor{ActorId: req.GetActorRef().GetName(), AteomPodIp: f.ResumeIP}}, nil
}

func (f *MockControlServer) SuspendActor(_ context.Context, req *ateapipb.SuspendActorRequest) (*ateapipb.SuspendActorResponse, error) {
	f.mu.Lock()
	f.suspendCalls = append(f.suspendCalls, req.GetActorRef().GetName())
	f.mu.Unlock()
	return &ateapipb.SuspendActorResponse{}, nil
}

// Calls returns copies of the recorded call lists.
func (f *MockControlServer) Calls() (create, resume, suspend []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.createCalls...),
		append([]string(nil), f.resumeCalls...),
		append([]string(nil), f.suspendCalls...)
}

// mockHarnessServer is an in-process proto.HarnessServiceServer standing in for
// the harness running inside an actor (substrate) or a local subprocess
// (antigravity). It records the start frame and emits its configured outputs
// followed by a terminal HarnessEnd.
type MockHarnessServer struct {
	proto.UnimplementedHarnessServiceServer

	// Outputs are the messages emitted (in a single Outputs frame) before the
	// terminal HarnessEnd. When nil, each input is echoed as "ack: <input>".
	Outputs []*proto.Message
	// FailConnect makes Connect return an RPC error before any frame.
	FailConnect bool
	// FailFrame makes Connect terminate the turn with HarnessEnd{STATE_FAILED}.
	FailFrame bool
	// ErrCode is the error code used by FailFrame.
	ErrCode int32
	// ErrMessage is the error text used by FailConnect/FailFrame.
	ErrMessage string

	mu               sync.Mutex
	gotConvID        string
	gotHarnessID     string
	gotHarnessConfig []byte
	gotInputs        []string
}

func (s *MockHarnessServer) Connect(stream proto.HarnessService_ConnectServer) error {
	if s.FailConnect {
		return status.Error(codes.Internal, s.ErrMessage)
	}

	req, err := stream.Recv()
	if err != nil {
		return err
	}

	var inputs []string
	for _, m := range req.GetStart().GetMessages() {
		if text := m.GetContent().GetText().GetText(); text != "" {
			inputs = append(inputs, text)
		}
	}
	s.mu.Lock()
	s.gotConvID = req.GetConversationId()
	s.gotHarnessID = req.GetHarnessId()
	s.gotHarnessConfig = req.GetStart().GetHarnessConfig()
	s.gotInputs = inputs
	s.mu.Unlock()

	convID := req.GetConversationId()
	if s.FailFrame {
		return stream.Send(&proto.HarnessResponse{
			ConversationId: convID,
			Type: &proto.HarnessResponse_End{
				End: &proto.HarnessEnd{
					State: proto.State_STATE_FAILED,
					Error: &proto.Error{
						Code:        s.ErrCode,
						Description: s.ErrMessage,
					},
				},
			},
		})
	}

	msgs := s.Outputs
	if msgs == nil {
		for _, in := range inputs {
			msgs = append(msgs, AssistantText("ack: "+in))
		}
	}
	if len(msgs) > 0 {
		if err := stream.Send(&proto.HarnessResponse{
			ConversationId: convID,
			Type: &proto.HarnessResponse_Outputs{
				Outputs: &proto.HarnessOutputs{Messages: msgs},
			},
		}); err != nil {
			return err
		}
	}
	return stream.Send(&proto.HarnessResponse{
		ConversationId: convID,
		Type:           &proto.HarnessResponse_End{End: &proto.HarnessEnd{State: proto.State_STATE_COMPLETED}},
	})
}

// Received returns a copy of the start frame the server received.
func (s *MockHarnessServer) Received() (convID, harnessID string, harnessConfig []byte, inputs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gotConvID, s.gotHarnessID, append([]byte(nil), s.gotHarnessConfig...), append([]string(nil), s.gotInputs...)
}

// mockHandler records the messages and completion streamed during a turn.
type MockHandler struct {
	mu       sync.Mutex
	messages []*proto.Message
	complete bool
}

var _ harness.Handler = (*MockHandler)(nil)

func (h *MockHandler) OnMessage(_ context.Context, _ string, msg *proto.Message) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, msg)
	return nil
}

func (h *MockHandler) OnComplete(_ context.Context, _ string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.complete = true
	return nil
}

func (h *MockHandler) IsDone() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.complete
}

// Collected returns a copy of the messages received via OnMessage.
func (h *MockHandler) Collected() []*proto.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*proto.Message(nil), h.messages...)
}

// Texts returns the text content of each received message, in order.
func (h *MockHandler) Texts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, m := range h.messages {
		out = append(out, m.GetContent().GetText().GetText())
	}
	return out
}

func AssistantText(text string) *proto.Message {
	return &proto.Message{
		Role:    "assistant",
		Content: &proto.Content{Type: &proto.Content_Text{Text: &proto.TextContent{Text: text}}},
	}
}

func UserText(text string) *proto.Message {
	return &proto.Message{
		Role:    "user",
		Content: &proto.Content{Type: &proto.Content_Text{Text: &proto.TextContent{Text: text}}},
	}
}

func ThoughtText(summary string) *proto.Message {
	return &proto.Message{
		Role: "model",
		Content: &proto.Content{
			Type: &proto.Content_Thought{
				Thought: &proto.ThoughtContent{
					Summary: []*proto.ThoughtSummaryContent{
						{Type: &proto.ThoughtSummaryContent_Text{Text: &proto.TextContent{Text: summary}}},
					},
				},
			},
		},
	}
}

// StartHarnessServer starts a HarnessService + health server (status SERVING)
// on a random local port and returns its address.
func StartHarnessServer(t *testing.T, srv *MockHarnessServer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	s := grpc.NewServer()
	proto.RegisterHarnessServiceServer(s, srv)
	hs := health.NewServer()
	hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(s, hs)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
	return lis.Addr().String()
}

// StartControlServer starts a mock Substrate Control server on a random local port.
func StartControlServer(t *testing.T, srv *MockControlServer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	s := grpc.NewServer()
	ateapipb.RegisterControlServer(s, srv)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
	return lis.Addr().String()
}
