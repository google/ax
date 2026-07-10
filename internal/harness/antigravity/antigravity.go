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
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/google/ax/internal/harness"
	"github.com/google/ax/internal/pythonsidecar"
	"github.com/google/ax/proto"
	"github.com/google/ax/python"
	"github.com/google/uuid"
)

// Compile-time interface assertions.
var _ harness.Harness = (*AntigravityHarness)(nil)
var _ harness.Execution = (*antigravityExecution)(nil)

// AntigravityHarness implements the Harness interface by connecting to the
// Antigravity Python agent server over gRPC.
type AntigravityHarness struct {
	address string
	sidecar *pythonsidecar.Sidecar // non-nil when this harness forked the process
}

// New creates a new AntigravityHarness. When autoStart is true, New forks the
// Antigravity Python sidecar and waits for it to become reachable. If
// stateDir is non-empty, it is passed to the sidecar as --state-dir; empty
// stateDir lets the sidecar apply its own default.
func New(ctx context.Context, address, stateDir string, autoStart bool) (*AntigravityHarness, error) {
	if address == "" {
		address = "127.0.0.1:50053"
	}
	h := &AntigravityHarness{address: address}
	if !autoStart {
		return h, nil
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("antigravity: invalid address %q: %w", address, err)
	}
	args := []string{"--host", host, "--port", port}
	if stateDir != "" {
		args = append(args, "--state-dir", stateDir)
	}
	cfg := pythonsidecar.Config{
		Module:    "python.antigravity.harness_server",
		Args:      args,
		ReadyFunc: pythonsidecar.TCPReady(address),
	}
	sidecar := pythonsidecar.New(cfg)
	path, err := pythonsidecar.Setup(ctx, pythonsidecar.SetupOptions{
		FS: python.FS,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to setup antigravity harness server assets: %w", err)
	}
	if err := sidecar.Start(ctx, path); err != nil {
		return nil, fmt.Errorf("failed to start antigravity harness server: %w", err)
	}
	h.sidecar = sidecar
	// Own the sidecar lifecycle here (no registry->controller->antigravity
	// teardown chain): on ctrl-C / SIGTERM, stop the Python process
	// directly. Mirrors cmd/ax/harness.go.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		_ = sidecar.Stop()
	}()
	return h, nil
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
	// A server that fails before reading the start frame makes Send/CloseSend
	// report io.EOF; the real status is surfaced by DrainStream's Recv below, so
	// only treat non-EOF errors as send failures.
	if err := stream.Send(start); err != nil && err != io.EOF {
		return fmt.Errorf("failed to send harness start: %w", err)
	}
	if err := stream.CloseSend(); err != nil && err != io.EOF {
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
