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

package antigravity

import (
	"context"
	"fmt"
	"sync"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/google/ax/internal/harness"
	"github.com/google/ax/proto"
	"github.com/google/uuid"
)

// Compile-time interface assertions.
var _ harness.Harness = (*AntigravityHarness)(nil)
var _ harness.Execution = (*antigravityExecution)(nil)

// AntigravityHarness implements the Harness interface by connecting to the
// Antigravity Python agent server over gRPC.
type AntigravityHarness struct {
	address string
}

// New creates a new AntigravityHarness with a configurable address.
// Address defaults to "127.0.0.1:50053" (gRPC TCP connection).
func New(address string) *AntigravityHarness {
	if address == "" {
		address = "127.0.0.1:50053"
	}
	return &AntigravityHarness{
		address: address,
	}
}

// Start implements Harness.Start.
func (h *AntigravityHarness) Start(ctx context.Context, conversationID string, harnessConfig []byte) (harness.Execution, error) {
	return &antigravityExecution{
		harness:        h,
		conversationID: conversationID,
		id:             uuid.NewString(),
		harnessConfig:  harnessConfig,
	}, nil
}

// antigravityExecution implements the Execution interface.
type antigravityExecution struct {
	harness        *AntigravityHarness
	conversationID string
	id             string
	harnessConfig  []byte

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
func (e *antigravityExecution) Run(ctx context.Context, handler harness.Handler) error {
	ctx, span := otel.Tracer("antigravity-harness").Start(ctx, "Run")
	defer span.End()

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
	dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	dialOpts = append(dialOpts, grpc.WithStatsHandler(otelgrpc.NewClientHandler()))
	conn, err := grpc.DialContext(ctx, e.harness.address, dialOpts...)
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
				HarnessConfig: e.harnessConfig,
				Messages:      inputs,
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
	return harness.DrainStream(ctx, stream, e.id, handler)
}

// Close implements Execution.Close.
func (e *antigravityExecution) Close(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	return nil
}
