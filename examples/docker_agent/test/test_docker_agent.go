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
	"fmt"
	"log"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/google/ax/proto"
)

func main() {
	fmt.Println("Connecting to DockerAgent at localhost:50052...")
	conn, err := grpc.NewClient("localhost:50052", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	client := proto.NewAgentServiceClient(conn)
	fmt.Println("Sending request to write a Dockerfile...")
	req := &proto.AgentRequest{
		ConversationId: "test-conversation-id",
		ExecId:         "test-exec-id",
		Start: &proto.AgentStart{
			AgentId: "DockerAgent",
			Messages: []*proto.Message{
				{
					Role: "user",
					Content: &proto.Content{
						Type: &proto.Content_Text{
							Text: &proto.TextContent{Text: "Write a Dockerfile for a simple Node.js app."},
						},
					},
				},
			},
		},
	}

	stream, err := client.Connect(context.Background(), req)
	if err != nil {
		log.Fatalf("Failed to open stream: %v", err)
	}

	fmt.Println("Waiting for response...")
	// Read responses
	for {
		resp, err := stream.Recv()
		if err != nil {
			log.Fatalf("Error receiving: %v", err)
		}
		if resp.GetEnd() != nil {
			fmt.Println("\n\n[Agent finished]")
			break
		}
		if outputs := resp.GetOutputs(); outputs != nil {
			for _, m := range outputs.Messages {
				if txt := m.GetContent().GetText(); txt != nil {
					fmt.Print(txt.Text)
				}
			}
		}
	}
}
