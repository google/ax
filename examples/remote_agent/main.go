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

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"

	"google.golang.org/grpc"

	"github.com/google/ax/proto"
)

const port = ":50051"

// server implements the AgentService gRPC server.
type server struct {
	proto.UnimplementedAgentServiceServer
}

// Connect implements server streaming for agent processing.
// This RPC is called by the controller when a session triggers this agent.
func (s *server) Connect(req *proto.AgentRequest, stream grpc.ServerStreamingServer[proto.AgentResponse]) error {
	start := req.GetStart()
	if start == nil {
		return errors.New("start is required")
	}

	var messages []*proto.Message
	for _, input := range start.Messages {
		textContent := input.GetContent().GetText()
		if textContent == nil {
			continue
		}
		messages = append(messages, input)
	}
	if len(messages) == 0 {
		return errors.New("no text inputs, cannot uppercase")
	}

	// We don't need to uppercase the whole history.
	// Only uppercase the last text message.
	lastMsg := messages[len(messages)-1]
	responseMsg := &proto.Message{
		Role: "assistant",
		Content: &proto.Content{
			Type: &proto.Content_Text{
				Text: &proto.TextContent{
					Text: strings.ToLower(lastMsg.GetContent().GetText().Text),
				},
			},
		},
	}

	if err := stream.Send(&proto.AgentResponse{
		ConversationId: req.ConversationId,
		ExecId:         req.ExecId,
		Type: &proto.AgentResponse_Outputs{
			Outputs: &proto.AgentOutputs{
				Messages: []*proto.Message{responseMsg},
			},
		},
	}); err != nil {
		return err
	}

	// Send AgentEnd to signal end of outputs.
	if err := stream.Send(&proto.AgentResponse{
		ConversationId: req.ConversationId,
		ExecId:         req.ExecId,
		Type: &proto.AgentResponse_End{
			End: &proto.AgentEnd{},
		},
	}); err != nil {
		return err
	}

	return nil
}

// HealthCheck checks if the agent is healthy.
func (s *server) HealthCheck(ctx context.Context, req *proto.HealthCheckRequest) (*proto.HealthCheckResponse, error) {
	log.Println("Health check requested")

	return &proto.HealthCheckResponse{
		Healthy: true,
		Message: "Agent is healthy",
	}, nil
}

func main() {
	fmt.Printf("Listening on port: %s\n", port)
	lis, err := net.Listen("tcp", port)
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	proto.RegisterAgentServiceServer(grpcServer, &server{})

	fmt.Println("\nAgent server is running...")
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("Failed to serve: %v", err)
	}
}
