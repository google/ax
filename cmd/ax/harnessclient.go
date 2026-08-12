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

// Package main implements a simple client for the fake HarnessService.
// It is intended for testing purposes only and should be replaced with
// the actual ax client implementation.
// TODO(wjjclaud): Update or replace this file with ax client implementation.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/google/ax/proto"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	harnessServerAddr string
	agentClientID     string
)

var harnessClientCmd = &cobra.Command{
	Use:    "harnessclient",
	Short:  "Run the harness client to connect to the server",
	Hidden: true,
	RunE:   runHarnessClient,
}

func init() {
	harnessClientCmd.Flags().StringVar(&harnessServerAddr, "server", "localhost:50053", "The server address for the gRPC HarnessService.")
	harnessClientCmd.Flags().StringVar(&agentClientID, "agent", "testharness", "The agent id to send on the request envelope.")
	rootCmd.AddCommand(harnessClientCmd)
}

func runHarnessClient(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	log.Printf("Connecting to HarnessService at %s...", harnessServerAddr)
	conn, err := grpc.NewClient(harnessServerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("failed to connect to server: %v", err)
	}
	defer conn.Close()

	client := proto.NewHarnessServiceClient(conn)
	stream, err := client.Connect(ctx)
	if err != nil {
		return fmt.Errorf("failed to call Connect: %v", err)
	}

	// Wait for user input from stdin if not specified, but for simple execution, send a single frame.
	var input string
	if len(args) > 0 {
		input = strings.Join(args, " ")
	} else {
		input = "Hello from harness client!"
	}

	// A single HarnessRequest{start} initiates the turn.
	start := &proto.HarnessRequest{
		ConversationId: uuid.NewString(),
		AgentId:        agentClientID,
		Type: &proto.HarnessRequest_Start{
			Start: &proto.HarnessStart{
				Steps: []*proto.Step{
					{
						Type: &proto.Step_Content{
							Content: &proto.ContentStep{
								Role: "user",
								Content: []*proto.Content{
									{Type: &proto.Content_Text{Text: &proto.TextContent{Text: input}}},
								},
							},
						},
					},
				},
			},
		},
	}
	if err := stream.Send(start); err != nil {
		return fmt.Errorf("failed to send start: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		return fmt.Errorf("failed to close send side: %v", err)
	}

	// Drain HarnessResponse frames until HarnessEnd / EOF.
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to receive response: %v", err)
		}
		switch payload := resp.Type.(type) {
		case *proto.HarnessResponse_Outputs:
			for i, step := range payload.Outputs.Steps {
				if contentStep := step.GetContent(); contentStep != nil {
					for _, c := range contentStep.Content {
						var text string
						if tb, ok := c.Type.(*proto.Content_Text); ok {
							text = tb.Text.Text
						}
						fmt.Printf("Server > step[%d] (%s): %s\n", i, contentStep.Role, text)
					}
				}
			}
		case *proto.HarnessResponse_End:
			if errDetail := payload.End.GetError(); errDetail != nil {
				fmt.Printf("Server > [end] state=%s error=(code=%d description=%q)\n",
					payload.End.GetState(), errDetail.GetCode(), errDetail.GetDescription())
			} else {
				fmt.Printf("Server > [end] state=%s\n", payload.End.GetState())
			}
		}
	}

	log.Println("Stream closed successfully by server.")
	return nil
}
