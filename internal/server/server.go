//go:build !harness

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

// Package server implements the gRPC server for AXService,
// exposing execution management and agent registration APIs.

package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/google/ax/internal/controller"
	"github.com/google/ax/proto"
)

// Server implements the AXService gRPC service.
type Server struct {
	proto.UnimplementedControllerServiceServer
	proto.UnimplementedConversationServiceServer

	controller *controller.Controller
	grpcServer *grpc.Server
	inFlight   map[string]struct{}
	inFlightMu sync.Mutex
}

// New creates a new controller server.
func New(c *controller.Controller) *Server {
	return &Server{
		controller: c,
		inFlight:   make(map[string]struct{}),
	}
}

// Exec executes a new agentic task with streaming responses.
func (s *Server) Exec(req *proto.ExecRequest, stream grpc.ServerStreamingServer[proto.ExecResponse]) error {
	ctx := stream.Context()
	slog.InfoContext(ctx, "Executing request",
		slog.String("request", req.String()),
	)

	inFlight, cleanup := s.markInFlight(req.ConversationId)
	if inFlight {
		return status.Errorf(codes.FailedPrecondition, "conversation %q is already in flight", req.ConversationId)
	}
	defer cleanup()

	outputHandler := controller.ExecHandler(func(resp *proto.ExecResponse) error {
		return stream.Send(resp)
	})
	err := s.controller.Exec(ctx, req, outputHandler)
	go suspendActor(req.ConversationId) // TODO(jbd): Move to an interceptor.
	return err
}

func (s *Server) DeleteConversation(ctx context.Context, req *proto.DeleteConversationRequest) (*proto.DeleteConversationResponse, error) {
	slog.InfoContext(ctx, "Deleting conversation...",
		slog.String("conversation_id", req.ConversationId))

	if req.ConversationId == "" {
		return nil, status.Errorf(codes.InvalidArgument, "conversation_id is required")
	}
	inFlight, cleanup := s.markInFlight(req.ConversationId)
	if inFlight {
		return nil, status.Errorf(codes.FailedPrecondition, "conversation %q is already in flight", req.ConversationId)
	}
	defer cleanup()

	if err := s.controller.Delete(ctx, req.ConversationId); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to delete conversation: %v", err)
	}
	go suspendActor(req.ConversationId) // TODO(jbd): Move to an interceptor.
	return &proto.DeleteConversationResponse{}, nil
}

// Serve starts the gRPC server on the specified address.
func (s *Server) Serve(address string, opts ...grpc.ServerOption) error {
	lis, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}

	opts = append(opts,
		grpc.ChainUnaryInterceptor(LoggingInterceptor),
		grpc.ChainStreamInterceptor(StreamLoggingInterceptor),
	)

	s.grpcServer = grpc.NewServer(opts...)
	proto.RegisterControllerServiceServer(s.grpcServer, s)
	proto.RegisterConversationServiceServer(s.grpcServer, s)

	// Register standard gRPC Health Check server
	hs := health.NewServer()
	hs.SetServingStatus("AX", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(s.grpcServer, hs)

	if err := s.grpcServer.Serve(lis); err != nil {
		return fmt.Errorf("failed to serve: %w", err)
	}
	return nil
}

// GracefulStop stops the gRPC server gracefully.
func (s *Server) GracefulStop() {
	slog.Info("Stopping server gracefully...")
	if s.controller != nil {
		s.controller.Close()
	}
	if s.grpcServer != nil {
		s.grpcServer.GracefulStop()
	}
}

func (s *Server) markInFlight(id string) (exists bool, cleanup func()) {
	s.inFlightMu.Lock()
	defer s.inFlightMu.Unlock()

	_, ok := s.inFlight[id]
	if ok {
		return true, func() {}
	}
	s.inFlight[id] = struct{}{}

	return false, func() {
		s.inFlightMu.Lock()
		delete(s.inFlight, id)
		s.inFlightMu.Unlock()
	}
}
