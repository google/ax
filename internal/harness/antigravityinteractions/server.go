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

package antigravityinteractions

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"

	"github.com/google/ax/internal/harness"
	"github.com/google/ax/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// server adapts the Antigravity Interactions harness (a Go harness.Harness) onto
// the gRPC HarnessService that the AX controller connects to. One server serves
// one actor's lifetime; substrate creates the actor per request.
type server struct {
	proto.UnimplementedHarnessServiceServer
	h harness.Harness
}

// Serve builds the Antigravity Interactions harness from cfg and serves the
// HarnessService (plus gRPC health) on host:port, and an HTTP /readyz endpoint on
// readyzPort that reflects this server's own serving state. It blocks until ctx
// is cancelled or a termination signal (SIGINT/SIGTERM) is received, then shuts
// down gracefully.
func Serve(ctx context.Context, cfg AntigravityInteractionsConfig, host string, port, readyzPort int) error {
	h, err := New(cfg)
	if err != nil {
		return fmt.Errorf("creating antigravity interactions harness: %w", err)
	}

	lis, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("listen on %s:%d: %w", host, port, err)
	}

	grpcServer := grpc.NewServer()
	proto.RegisterHarnessServiceServer(grpcServer, &server{h: h})

	// gRPC health: report SERVING for the default service so the controller's
	// health checks pass.
	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcServer, hs)

	// Go-based readiness: /readyz is OK once this server is up and serving.
	readyzSrv := serveReadyz(host, readyzPort)

	// Shut down on ctx cancellation or termination signals.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Printf("antigravity interactions harness: shutting down")
		if readyzSrv != nil {
			_ = readyzSrv.Close()
		}
		grpcServer.GracefulStop()
	}()

	log.Printf("antigravity interactions harness serving on %s:%d (readyz :%d)", host, port, readyzPort)
	if err := grpcServer.Serve(lis); err != nil {
		return fmt.Errorf("harness gRPC server: %w", err)
	}
	return nil
}

// serveReadyz starts an HTTP server exposing /readyz that returns 200 as long as
// this process's gRPC server is running. Readiness is intrinsic to this server.
func serveReadyz(host string, readyzPort int) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	srv := &http.Server{
		Addr:    net.JoinHostPort(host, strconv.Itoa(readyzPort)),
		Handler: mux,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("readyz server on %s exited: %v", srv.Addr, err)
		}
	}()
	return srv
}

// Connect drives one harness execution. Per the HarnessService contract, the
// client sends HarnessRequest{start} (and may send one HarnessRequest{cancel}
// mid-stream); the server streams zero or more HarnessResponse{outputs} frames
// terminated by exactly one HarnessResponse{end}.
func (s *server) Connect(stream proto.HarnessService_ConnectServer) error {
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	// The first frame must be a start.
	req, err := stream.Recv()
	if err != nil {
		return err
	}
	start := req.GetStart()
	if start == nil {
		return fmt.Errorf("first HarnessRequest must be a start frame")
	}
	convID := req.GetConversationId()

	exec, err := s.h.Start(ctx, convID, start.GetHarnessConfig())
	if err != nil {
		return sendEnd(stream, convID, proto.State_STATE_FAILED, err)
	}
	defer func() { _ = exec.Close(context.WithoutCancel(ctx)) }()

	if len(start.GetMessages()) > 0 {
		if err := exec.Queue(ctx, start.GetMessages()...); err != nil {
			return sendEnd(stream, convID, proto.State_STATE_FAILED, err)
		}
	}

	// Watch for a mid-stream cancel frame; it cancels the run's context.
	//
	// Note: mid-run steering (injecting extra human input while a turn is running)
	// is intentionally NOT supported here. The execution can accept steering via
	// Queue, but the HarnessRequest oneof only defines {start, cancel} -- there is
	// no wire frame to carry additional input after start -- so a client cannot
	// deliver steering over Connect. Multi-turn conversation is instead expressed
	// as separate Connect executions that resume via the persisted cursor. Any
	// non-cancel frame received here is therefore ignored.
	go func() {
		for {
			r, err := stream.Recv()
			if err != nil {
				return // stream closed or errored; run ends by other means
			}
			if r.GetCancel() != nil {
				cancel()
				return
			}
		}
	}()

	// Run the turn, forwarding each agent message as an outputs frame.
	handler := &streamHandler{stream: stream, convID: convID}
	if err := exec.Run(ctx, handler); err != nil {
		if ctx.Err() != nil {
			return sendEnd(stream, convID, proto.State_STATE_CANCELED, ctx.Err())
		}
		return sendEnd(stream, convID, proto.State_STATE_FAILED, err)
	}
	if serr := handler.err(); serr != nil {
		return serr // failed while sending an outputs frame; stream is broken
	}
	return sendEnd(stream, convID, proto.State_STATE_COMPLETED, nil)
}

// streamHandler forwards each message the harness produces during a turn as a
// HarnessResponse{outputs} frame.
type streamHandler struct {
	stream proto.HarnessService_ConnectServer
	convID string

	mu      sync.Mutex
	sendErr error
}

var _ harness.Handler = (*streamHandler)(nil)

func (h *streamHandler) OnMessage(_ context.Context, _ string, msg *proto.Message) error {
	err := h.stream.Send(&proto.HarnessResponse{
		ConversationId: h.convID,
		Type: &proto.HarnessResponse_Outputs{
			Outputs: &proto.HarnessOutputs{Messages: []*proto.Message{msg}},
		},
	})
	if err != nil {
		h.mu.Lock()
		h.sendErr = err
		h.mu.Unlock()
	}
	return err
}

func (h *streamHandler) OnComplete(_ context.Context, _ string) error { return nil }

func (h *streamHandler) err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sendErr
}

// sendEnd sends the single terminal HarnessResponse{end} frame. If err is
// non-nil, it is attached as the terminal Error.
func sendEnd(stream proto.HarnessService_ConnectServer, convID string, state proto.State, err error) error {
	end := &proto.HarnessEnd{State: state}
	if err != nil {
		end.Error = &proto.Error{Description: err.Error()}
	}
	return stream.Send(&proto.HarnessResponse{
		ConversationId: convID,
		Type:           &proto.HarnessResponse_End{End: end},
	})
}
