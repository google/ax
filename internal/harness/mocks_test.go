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

// Shared in-process mocks for the harness tests: a mock Substrate Control server
// (the substrate control plane), a mock HarnessService server (the harness
// inside an actor), a recording Handler, and message builders.

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
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
type mockControlServer struct {
	ateapipb.UnimplementedControlServer

	mu           sync.Mutex
	createCalls  []string
	resumeCalls  []string
	suspendCalls []string

	createErr      error  // returned from CreateActor when non-nil
	resumeIP       string // AteomPodIp returned from ResumeActor
	resumeNilActor bool   // when true, ResumeActor returns a nil Actor
}

func (f *mockControlServer) CreateActor(_ context.Context, req *ateapipb.CreateActorRequest) (*ateapipb.CreateActorResponse, error) {
	f.mu.Lock()
	f.createCalls = append(f.createCalls, req.GetActorId())
	f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &ateapipb.CreateActorResponse{Actor: &ateapipb.Actor{ActorId: req.GetActorId()}}, nil
}

func (f *mockControlServer) ResumeActor(_ context.Context, req *ateapipb.ResumeActorRequest) (*ateapipb.ResumeActorResponse, error) {
	f.mu.Lock()
	f.resumeCalls = append(f.resumeCalls, req.GetActorId())
	f.mu.Unlock()
	if f.resumeNilActor {
		return &ateapipb.ResumeActorResponse{}, nil
	}
	return &ateapipb.ResumeActorResponse{Actor: &ateapipb.Actor{ActorId: req.GetActorId(), AteomPodIp: f.resumeIP}}, nil
}

func (f *mockControlServer) SuspendActor(_ context.Context, req *ateapipb.SuspendActorRequest) (*ateapipb.SuspendActorResponse, error) {
	f.mu.Lock()
	f.suspendCalls = append(f.suspendCalls, req.GetActorId())
	f.mu.Unlock()
	return &ateapipb.SuspendActorResponse{}, nil
}

// calls returns copies of the recorded call lists.
func (f *mockControlServer) calls() (create, resume, suspend []string) {
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
type mockHarnessServer struct {
	proto.UnimplementedHarnessServiceServer

	// outputs are the messages emitted (in a single Outputs frame) before the
	// terminal HarnessEnd. When nil, each input is echoed as "ack: <input>".
	outputs []*proto.Message
	// failConnect makes Connect return an RPC error before any frame.
	failConnect bool
	// failFrame makes Connect terminate the turn with HarnessEnd{STATE_FAILED}.
	failFrame bool
	// errMessage is the error text used by failConnect/failFrame.
	errMessage string

	mu               sync.Mutex
	gotConvID        string
	gotHarnessID     string
	gotHarnessConfig []byte
	gotInputs        []string
}

func (s *mockHarnessServer) Connect(stream proto.HarnessService_ConnectServer) error {
	if s.failConnect {
		return status.Error(codes.Internal, s.errMessage)
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
	if s.failFrame {
		return stream.Send(&proto.HarnessResponse{
			ConversationId: convID,
			Type: &proto.HarnessResponse_End{
				End: &proto.HarnessEnd{State: proto.State_STATE_FAILED, ErrorMessage: s.errMessage},
			},
		})
	}

	msgs := s.outputs
	if msgs == nil {
		for _, in := range inputs {
			msgs = append(msgs, assistantText("ack: "+in))
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

// received returns a copy of the start frame the server received.
func (s *mockHarnessServer) received() (convID, harnessID string, harnessConfig []byte, inputs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gotConvID, s.gotHarnessID, append([]byte(nil), s.gotHarnessConfig...), append([]string(nil), s.gotInputs...)
}

// mockHandler records the messages and completion streamed during a turn.
type mockHandler struct {
	mu       sync.Mutex
	messages []*proto.Message
	complete bool
}

func (h *mockHandler) OnMessage(_ context.Context, _ string, msg *proto.Message) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, msg)
	return nil
}

func (h *mockHandler) OnComplete(_ context.Context, _ string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.complete = true
	return nil
}

func (h *mockHandler) isDone() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.complete
}

// collected returns a copy of the messages received via OnMessage.
func (h *mockHandler) collected() []*proto.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*proto.Message(nil), h.messages...)
}

// texts returns the text content of each received message, in order.
func (h *mockHandler) texts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, m := range h.messages {
		out = append(out, m.GetContent().GetText().GetText())
	}
	return out
}

func assistantText(text string) *proto.Message {
	return &proto.Message{
		Role:    "assistant",
		Content: &proto.Content{Type: &proto.Content_Text{Text: &proto.TextContent{Text: text}}},
	}
}

func userText(text string) *proto.Message {
	return &proto.Message{
		Role:    "user",
		Content: &proto.Content{Type: &proto.Content_Text{Text: &proto.TextContent{Text: text}}},
	}
}

func thoughtText(summary string) *proto.Message {
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

// startHarnessServer starts a HarnessService + health server (status SERVING)
// on a random local port and returns its address.
func startHarnessServer(t *testing.T, srv *mockHarnessServer) string {
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

// startControlServer starts a mock Substrate Control server on a random local port.
func startControlServer(t *testing.T, srv *mockControlServer) string {
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
