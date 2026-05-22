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
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/google/ax/internal/experimental/k8s/ate"
	"github.com/google/ax/proto"
	"github.com/google/uuid"
)

// SubstrateHarness manages execution in a SubstrATE sandboxed actor over gRPC HarnessService.
type SubstrateHarness struct {
	ateClient *ate.Client
	port      int
	dialOpts  []grpc.DialOption
}

// NewSubstrateHarness creates a new SubstrateHarness.
func NewSubstrateHarness(endpoint string, namespace string, template string, port int, opts ...grpc.DialOption) (*SubstrateHarness, error) {
	if port == 0 {
		port = 50053 // Default HarnessService port
	}
	if namespace == "" {
		namespace = "ax"
	}
	if template == "" {
		template = "ax-harness-template"
	}
	client, err := ate.NewClient(namespace, template, endpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to create ATE client: %w", err)
	}
	if len(opts) == 0 {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	return &SubstrateHarness{
		ateClient: client,
		port:      port,
		dialOpts:  opts,
	}, nil
}

// Start implements Harness interface. It creates/resumes the target actor.
func (h *SubstrateHarness) Start(ctx context.Context, conversationID string) (Execution, error) {
	if conversationID == "" {
		return nil, errors.New("SubstrateHarness needs valid conversationID")
	}

	resp, err := h.ateClient.CreateActor(ctx, conversationID)
	if err != nil {
		return nil, fmt.Errorf("failed to create substrate actor %s: %w", conversationID, err)
	}
	actor := resp.Actor
	if actor == nil {
		return nil, fmt.Errorf("received nil actor in response for %s", conversationID)
	}
	if actor.AteomPodIp == "" {
		return nil, fmt.Errorf("actor %s has no active worker IP address", conversationID)
	}

	// 2. Establish connection to the actor's worker IP
	workerAddr := fmt.Sprintf("%s:%d", actor.AteomPodIp, h.port)
	conn, err := grpc.NewClient(workerAddr, h.dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to dial remote harness service at %s: %w", workerAddr, err)
	}

	return &substrateExecution{
		harness:        h,
		conversationID: conversationID,
		execID:         uuid.NewString(),
		conn:           conn,
		client:         proto.NewHarnessServiceClient(conn),
	}, nil
}

type substrateExecution struct {
	harness        *SubstrateHarness
	conversationID string
	execID         string
	conn           *grpc.ClientConn
	client         proto.HarnessServiceClient

	mu      sync.Mutex
	pending []*proto.Message
}

func (e *substrateExecution) ID() string {
	return e.execID
}

func (e *substrateExecution) Queue(ctx context.Context, msg ...*proto.Message) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pending = append(e.pending, msg...)
	return nil
}

func (e *substrateExecution) Run(ctx context.Context, handler Handler) error {
	e.mu.Lock()
	inputs := e.pending
	e.pending = nil
	e.mu.Unlock()

	stream, err := e.client.Connect(ctx)
	if err != nil {
		return fmt.Errorf("failed to open harness service stream: %w", err)
	}

	// Send inputs
	err = stream.Send(&proto.HarnessMessage{
		Messages: inputs,
	})
	if err != nil {
		return fmt.Errorf("failed to send harness inputs: %w", err)
	}

	// Close send direction to trigger server processing
	if err := stream.CloseSend(); err != nil {
		return fmt.Errorf("failed to close stream send direction: %w", err)
	}

	// Receive response stream
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("error receiving from harness stream: %w", err)
		}
		for _, m := range resp.Messages {
			if err := handler.OnMessage(ctx, e.execID, m); err != nil {
				return err
			}
		}
	}

	return handler.OnComplete(ctx, e.execID)
}

func (e *substrateExecution) Close(ctx context.Context) error {
	// Close connection
	if e.conn != nil {
		e.conn.Close()
	}

	// Suspend actor to return resource to standard standby pool
	log.Printf("Suspending SubstrATE actor for conversation %s (execution %s)", e.conversationID, e.execID)
	suspendCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := e.harness.ateClient.SuspendActor(suspendCtx, e.conversationID); err != nil {
		log.Printf("Failed to suspend actor %s: %v", e.conversationID, err)
	}

	return nil
}
