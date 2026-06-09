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
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/google/ax/proto"
	"github.com/google/uuid"
)

// Compile-time interface assertions.
var _ Harness = (*AntigravityHarness)(nil)
var _ Execution = (*antigravityExecution)(nil)

// AntigravityHarness implements the Harness interface by connecting to the
// Antigravity Python agent server over gRPC.
type AntigravityHarness struct {
	address string
}

// NewAntigravityHarness creates a new AntigravityHarness with a configurable address.
// Address defaults to "localhost:50053" (gRPC TCP connection).
func NewAntigravityHarness(address string) *AntigravityHarness {
	if address == "" {
		address = "localhost:50053"
	}
	return &AntigravityHarness{
		address: address,
	}
}

// Start implements Harness.Start.
func (h *AntigravityHarness) Start(ctx context.Context, conversationID string) (Execution, error) {
	return &antigravityExecution{
		harness:        h,
		conversationID: conversationID,
		id:             uuid.NewString(),
	}, nil
}

// antigravityExecution implements the Execution interface.
type antigravityExecution struct {
	harness        *AntigravityHarness
	conversationID string
	id             string

	mu     sync.Mutex
	queued []*proto.Message
	closed bool
}

// ID implements Execution.ID.
func (e *antigravityExecution) ID() string {
	return e.id
}

// Queue implements Execution.Queue.
func (e *antigravityExecution) Queue(ctx context.Context, msg ...*proto.Message) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return fmt.Errorf("execution session already closed")
	}
	e.queued = append(e.queued, msg...)
	return nil
}

// Run executes the turn over gRPC bidirectional streaming and forwards events to the handler.
func (e *antigravityExecution) Run(ctx context.Context, handler Handler) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return fmt.Errorf("execution session already closed")
	}
	// Retrieve queued inputs
	inputs := e.queued
	e.queued = nil
	e.mu.Unlock()

	if len(inputs) == 0 {
		return fmt.Errorf("no input messages queued for execution turn")
	}

	// 1. Connect to the gRPC server
	conn, err := grpc.DialContext(ctx, e.harness.address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("failed to connect to gRPC harness server at %s: %w", e.harness.address, err)
	}
	defer conn.Close()

	// 2. Create HarnessService client.
	client := proto.NewHarnessServiceClient(conn)

	// 3. Build standard HarnessRequest.
	start := &proto.HarnessRequest{
		ConversationId: e.conversationID,
		HarnessId:      "antigravity",
		Type: &proto.HarnessRequest_Start{
			Start: &proto.HarnessStart{
				Messages: inputs,
			},
		},
	}

	// 4. Call Connect to start bidirectional streaming
	stream, err := client.Connect(ctx)
	if err != nil {
		return fmt.Errorf("failed to call gRPC HarnessService.Connect: %w", err)
	}
	if err := stream.Send(start); err != nil {
		return fmt.Errorf("failed to send harness start: %w", err)
	}
	if err := stream.CloseSend(); err != nil {
		return fmt.Errorf("failed to close stream send direction: %w", err)
	}

	// 5. Stream responses and trigger callbacks
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("gRPC harness streaming failure: %w", err)
		}

		switch payload := resp.Type.(type) {
		case *proto.HarnessResponse_Outputs:
			for _, outMsg := range payload.Outputs.Messages {
				if err := handler.OnMessage(ctx, e.id, outMsg); err != nil {
					return fmt.Errorf("failed to dispatch streamed output: %w", err)
				}
			}
		case *proto.HarnessResponse_End:
			if payload.End.GetState() == proto.State_STATE_FAILED {
				return fmt.Errorf("harness failed: %s", payload.End.GetErrorMessage())
			}
			return handler.OnComplete(ctx, e.id)
		}
	}

	return nil
}

// Close implements Execution.Close.
func (e *antigravityExecution) Close(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	return nil
}
