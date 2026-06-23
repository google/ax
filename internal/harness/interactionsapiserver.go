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

// InteractionsAPIServer exposes the Interactions API harness over the
// HarnessService gRPC contract, so the harness's main logic can run as a
// standalone server (e.g. isolated on substrate) that any HarnessService client
// -- such as AntigravityHarness or SubstrateHarness -- can dial.
//
// It is a thin adapter: each Connect stream creates an InteractionsAPIHarness
// Execution, queues the start messages, drives Run to completion, forwards the
// agent's output messages as HarnessResponse{outputs}, and terminates the stream
// with exactly one HarnessResponse{end}. The actual work (driving the
// Interactions API, executing built-in and third-party tools) lives in
// InteractionsAPIHarness; this server just maps it onto the wire protocol.

import (
	"context"
	"fmt"

	"github.com/google/ax/proto"
)

// InteractionsAPIServer implements proto.HarnessServiceServer backed by an
// InteractionsAPIHarness.
type InteractionsAPIServer struct {
	proto.UnimplementedHarnessServiceServer
	harness *InteractionsAPIHarness
}

// NewInteractionsAPIServer creates a HarnessService server backed by a harness
// built from the given config.
func NewInteractionsAPIServer(cfg InteractionsAPIConfig) *InteractionsAPIServer {
	return &InteractionsAPIServer{harness: NewInteractionsAPIHarness(cfg)}
}

// Connect implements proto.HarnessServiceServer.Connect. It drives one harness
// execution: read the initial HarnessRequest{start}, run the harness, stream
// outputs, and end with a single HarnessEnd.
func (s *InteractionsAPIServer) Connect(stream proto.HarnessService_ConnectServer) error {
	ctx := stream.Context()

	// 1. Read the first request, which must be a start frame.
	req, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("receiving initial harness request: %w", err)
	}
	start := req.GetStart()
	if start == nil {
		return sendEnd(stream, req.GetConversationId(),
			proto.State_STATE_FAILED, "first HarnessRequest must contain a start frame")
	}
	conversationID := req.GetConversationId()

	// 2. Start an execution and queue the start messages.
	exec, err := s.harness.Start(ctx, conversationID)
	if err != nil {
		return sendEnd(stream, conversationID, proto.State_STATE_FAILED,
			fmt.Sprintf("starting execution: %v", err))
	}
	defer exec.Close(ctx)
	if err := exec.Queue(ctx, start.GetMessages()...); err != nil {
		return sendEnd(stream, conversationID, proto.State_STATE_FAILED,
			fmt.Sprintf("queueing start messages: %v", err))
	}

	// 3. Drive the harness, forwarding output messages as they stream.
	//
	// TODO: support steering (mid-run input). The harness Execution is already
	// steering-capable -- Run drains newly Queue'd input at each interaction gap
	// -- but this server only reads the single start frame above and then blocks
	// in Run, so any frames the client sends mid-execution are never received or
	// queued. Wiring steering would require a concurrent goroutine reading
	// stream.Recv() during Run and calling exec.Queue(...) for each new input
	// (and handling HarnessCancel).
	//
	// This is intentionally deferred until the wire contract is settled: the
	// HarnessRequest oneof currently has only `start` and `cancel`, and there is
	// no frame that clearly means "continue/steer" (HarnessStart is named
	// "start", not "continue"). We should not overload `start` to carry mid-run
	// input; a dedicated frame (e.g. HarnessContinue) should be defined first.
	handler := &streamingHandler{stream: stream, conversationID: conversationID}
	if err := exec.Run(ctx, handler); err != nil {
		return sendEnd(stream, conversationID, proto.State_STATE_FAILED, err.Error())
	}

	// 4. Terminal success frame.
	return sendEnd(stream, conversationID, proto.State_STATE_COMPLETED, "")
}

// streamingHandler implements Handler by forwarding each output message to the
// gRPC stream as a HarnessResponse{outputs}. Completion is signaled by Connect
// itself (via the terminal HarnessEnd), so OnComplete is a no-op here.
type streamingHandler struct {
	stream         proto.HarnessService_ConnectServer
	conversationID string
}

func (h *streamingHandler) OnMessage(ctx context.Context, execID string, msg *proto.Message) error {
	return h.stream.Send(&proto.HarnessResponse{
		ConversationId: h.conversationID,
		Type: &proto.HarnessResponse_Outputs{
			Outputs: &proto.HarnessOutputs{Messages: []*proto.Message{msg}},
		},
	})
}

func (h *streamingHandler) OnComplete(ctx context.Context, execID string) error {
	// The terminal HarnessEnd is sent by Connect after Run returns.
	return nil
}

// sendEnd sends the terminal HarnessResponse{end} frame.
func sendEnd(stream proto.HarnessService_ConnectServer, conversationID string, state proto.State, errMsg string) error {
	return stream.Send(&proto.HarnessResponse{
		ConversationId: conversationID,
		Type: &proto.HarnessResponse_End{
			End: &proto.HarnessEnd{State: state, ErrorMessage: errMsg},
		},
	})
}
