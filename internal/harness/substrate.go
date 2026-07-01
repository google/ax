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
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/google/ax/internal/k8s/ate"
	"github.com/google/ax/proto"
	"github.com/google/uuid"
)

// healthCheckTimeout defines the maximum time Start waits for a freshly
// created/resumed actor's harness to become reachable and ready.
const healthCheckTimeout = 60 * time.Second

// SubstrateHarness manages execution in a SubstrATE sandboxed actor over gRPC HarnessService.
type SubstrateHarness struct {
	harnessID string
	ateClient *ate.Client
	port      int
	dialOpts  []grpc.DialOption
}

// NewSubstrateHarness creates a new SubstrateHarness.
func NewSubstrateHarness(harnessID string, endpoint string, namespace string, template string, port int, opts ...grpc.DialOption) (*SubstrateHarness, error) {
	if port == 0 {
		port = 50053 // Default HarnessService port
	}
	if namespace == "" {
		namespace = "ax"
	}
	if template == "" {
		template = "ax-harness-template"
	}
	controlCreds := grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true}))
	client, err := ate.NewClient(namespace, template, endpoint, controlCreds)
	if err != nil {
		return nil, fmt.Errorf("failed to create ATE client: %w", err)
	}
	if len(opts) == 0 {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	opts = append(opts, grpc.WithStatsHandler(otelgrpc.NewClientHandler()))
	return &SubstrateHarness{
		harnessID: harnessID,
		ateClient: client,
		port:      port,
		dialOpts:  opts,
	}, nil
}

// Start implements Harness interface. It creates/resumes the target actor.
func (h *SubstrateHarness) Start(ctx context.Context, conversationID string, harnessConfig []byte) (Execution, error) {
	if conversationID == "" {
		return nil, errors.New("SubstrateHarness needs valid conversationID")
	}

	// CreateActor is idempotent here: on follow-up turns the actor was created
	// (and suspended) on a previous turn, so AlreadyExists is expected and fine.
	if _, err := h.ateClient.CreateActor(ctx, conversationID); err != nil && status.Code(err) != codes.AlreadyExists {
		return nil, fmt.Errorf("failed to create substrate actor %s: %w", conversationID, err)
	}

	// Resume the actor so it is scheduled onto a worker and gets a routable IP.
	resumeResp, err := h.ateClient.ResumeActor(ctx, conversationID)
	if err != nil {
		return nil, fmt.Errorf("failed to resume substrate actor %s: %w", conversationID, err)
	}
	actor := resumeResp.Actor
	if actor == nil {
		return nil, fmt.Errorf("received nil actor in response for %s", conversationID)
	}
	if actor.AteomPodIp == "" {
		return nil, fmt.Errorf("actor %s has no active worker IP address", conversationID)
	}

	// Establish connection to the actor's worker IP
	workerAddr := fmt.Sprintf("%s:%d", actor.AteomPodIp, h.port)
	conn, err := grpc.NewClient(workerAddr, h.dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to dial remote harness service at %s: %w", workerAddr, err)
	}

	// Wait for the harness to be reachable and ready before handing back the
	// execution.
	if err := waitForHealthy(ctx, conn, healthCheckTimeout); err != nil {
		conn.Close()
		return nil, fmt.Errorf("harness for %s not ready at %s: %w", conversationID, workerAddr, err)
	}

	return &substrateExecution{
		harness:        h,
		conversationID: conversationID,
		execID:         uuid.NewString(),
		conn:           conn,
		client:         proto.NewHarnessServiceClient(conn),
		harnessConfig:  harnessConfig,
	}, nil
}

// waitForHealthy blocks until the harness behind conn reports SERVING via the
// standard gRPC health protocol until timeout. A harness that is reachable
// but does not implement the health service (Unimplemented) is treated as
// ready; connection failures (Unavailable) and NOT_SERVING are retried.
func waitForHealthy(ctx context.Context, conn *grpc.ClientConn, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := grpc_health_v1.NewHealthClient(conn)
	const maxBackoff = 2 * time.Second
	backoff := 100 * time.Millisecond
	for {
		resp, err := client.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: ""})
		if err == nil && resp.GetStatus() == grpc_health_v1.HealthCheckResponse_SERVING {
			return nil
		}
		if status.Code(err) == codes.Unimplemented {
			// Reachable but no health service: the port is up, proceed.
			return nil
		}

		select {
		case <-ctx.Done():
			if err != nil {
				return fmt.Errorf("harness not healthy within %s: %w", timeout, err)
			}
			return fmt.Errorf("harness not healthy within %s (last status: %s)", timeout, resp.GetStatus())
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

type substrateExecution struct {
	harness        *SubstrateHarness
	conversationID string
	execID         string
	conn           *grpc.ClientConn
	client         proto.HarnessServiceClient
	harnessConfig  []byte

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
	ctx, span := otel.Tracer("substrate-harness").Start(ctx, "Run")
	defer span.End()

	e.mu.Lock()
	inputs := e.pending
	e.pending = nil
	e.mu.Unlock()

	stream, err := e.client.Connect(ctx)
	if err != nil {
		return fmt.Errorf("failed to open harness service stream: %w", err)
	}

	// Send a HarnessRequest to initiate the turn.
	start := &proto.HarnessRequest{
		ConversationId: e.conversationID,
		HarnessId:      e.harness.harnessID,
		Type: &proto.HarnessRequest_Start{
			Start: &proto.HarnessStart{
				HarnessConfig: e.harnessConfig,
				Messages:      inputs,
			},
		},
	}
	if err := stream.Send(start); err != nil {
		return fmt.Errorf("failed to send harness start: %w", err)
	}

	// Close send direction to trigger server processing.
	if err := stream.CloseSend(); err != nil {
		return fmt.Errorf("failed to close stream send direction: %w", err)
	}

	// Drain HarnessResponse frames until the terminal HarnessEnd.
	return drainStream(ctx, stream, e.execID, handler)
}

func (e *substrateExecution) Close(ctx context.Context) error {
	// Close connection
	if e.conn != nil {
		e.conn.Close()
	}

	// Suspend actor to return resource to standard standby pool
	slog.InfoContext(ctx, "Suspending SubstrATE actor",
		slog.String("conversation_id", e.conversationID),
		slog.String("exec_id", e.execID),
	)
	suspendCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := e.harness.ateClient.SuspendActor(suspendCtx, e.conversationID); err != nil {
		slog.ErrorContext(ctx, "Failed to suspend SubstrATE actor",
			slog.String("conversation_id", e.conversationID),
			slog.Any("error", err),
		)
	}

	return nil
}
