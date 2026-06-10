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

// Package main implements a demo HarnessService.
// It is intended for testing purposes only and should be replaced with a real
// implementation in production.
// TODO(wjjclaud): Replace this file with a real harness implementation.
package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/google/ax/proto"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
)

var (
	harnessPort int
)

var harnessCmd = &cobra.Command{
	Use:    "harness",
	Short:  "Run the harness gRPC server",
	Hidden: true,
	RunE:   runHarness,
}

func init() {
	harnessCmd.Flags().IntVar(&harnessPort, "port", 50053, "The port for the gRPC HarnessService to listen on")
	rootCmd.AddCommand(harnessCmd)
}

func runHarness(cmd *cobra.Command, args []string) error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", harnessPort))
	if err != nil {
		return fmt.Errorf("failed to listen on port :%d: %w", harnessPort, err)
	}

	// Start gRPC Server
	grpcServer := grpc.NewServer()
	harnessServer := NewHarnessServiceServer()
	proto.RegisterHarnessServiceServer(grpcServer, harnessServer)

	// Serve the standard gRPC health protocol.
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)

	// Graceful shutdown handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("\nReceived shutdown signal, stopping gRPC HarnessService server gracefully...")
		grpcServer.GracefulStop()
	}()

	log.Printf("gRPC HarnessService listening on port :%d...\n", harnessPort)
	if err := grpcServer.Serve(lis); err != nil {
		return fmt.Errorf("failed to serve gRPC: %w", err)
	}
	return nil
}

// conversationState is the per-conversation state the stub keeps in process memory.
// On substrate this state is preserved across turns by snapshot/suspend/resume.
type conversationState struct {
	turns   int
	history []string
}

// HarnessServiceServer implements the gRPC proto.HarnessServiceServer interface.
type HarnessServiceServer struct {
	proto.UnimplementedHarnessServiceServer

	mu            sync.Mutex
	conversations map[string]*conversationState
}

// NewHarnessServiceServer creates a new HarnessServiceServer.
func NewHarnessServiceServer() *HarnessServiceServer {
	return &HarnessServiceServer{conversations: make(map[string]*conversationState)}
}

// Connect implements one HarnessService turn. It reads the initial
// HarnessRequest{start} frame, then replies with a "hello world (turn N)"
// frame, an echo of each input, and a recap of the inputs from prior turns,
// terminating with HarnessEnd{STATE_COMPLETED}.
//
// The per-conversation turn count and input history are kept in process
// memory and persist across turns.
func (s *HarnessServiceServer) Connect(stream proto.HarnessService_ConnectServer) error {
	req, err := stream.Recv()
	if err != nil {
		return err
	}

	convID := req.GetConversationId()
	if req.GetStart() == nil {
		return stream.Send(&proto.HarnessResponse{
			ConversationId: convID,
			Type: &proto.HarnessResponse_End{
				End: &proto.HarnessEnd{
					State:        proto.State_STATE_FAILED,
					ErrorMessage: "expected HarnessRequest{start} as the first frame",
				},
			},
		})
	}

	// Collect this turn's input text(s).
	var inputs []string
	for _, m := range req.GetStart().GetMessages() {
		if text := m.GetContent().GetText().GetText(); text != "" {
			inputs = append(inputs, text)
		}
	}

	// Update per-conversation state held in process memory.
	s.mu.Lock()
	st := s.conversations[convID]
	if st == nil {
		st = &conversationState{}
		s.conversations[convID] = st
	}
	st.turns++
	turn := st.turns
	prior := ""
	if len(st.history) > 0 {
		prior = st.history[len(st.history)-1]
	}
	st.history = append(st.history, inputs...)
	s.mu.Unlock()

	// Reply: turn number, this turn's inputs, and the remembered prior inputs.
	if err := stream.Send(textOutput(convID, fmt.Sprintf("hello world (turn %d)", turn))); err != nil {
		return err
	}
	for _, in := range inputs {
		if err := stream.Send(textOutput(convID, "received: "+in)); err != nil {
			return err
		}
	}
	if prior != "" {
		if err := stream.Send(textOutput(convID, "previously you said: "+prior)); err != nil {
			return err
		}
	}

	return stream.Send(&proto.HarnessResponse{
		ConversationId: convID,
		Type: &proto.HarnessResponse_End{
			End: &proto.HarnessEnd{State: proto.State_STATE_COMPLETED},
		},
	})
}

// textOutput builds a HarnessResponse carrying a single assistant text Message.
func textOutput(convID, text string) *proto.HarnessResponse {
	return &proto.HarnessResponse{
		ConversationId: convID,
		Type: &proto.HarnessResponse_Outputs{
			Outputs: &proto.HarnessOutputs{
				Messages: []*proto.Message{
					{
						Role: "assistant",
						Content: &proto.Content{
							Type: &proto.Content_Text{
								Text: &proto.TextContent{Text: text},
							},
						},
					},
				},
			},
		},
	}
}
