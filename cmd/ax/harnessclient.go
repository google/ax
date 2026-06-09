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
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/google/ax/proto"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	harnessServerAddr string
	harnessClientID   string
)

var harnessClientCmd = &cobra.Command{
	Use:    "harnessclient",
	Short:  "Run the harness client to connect to the server",
	Hidden: true,
	RunE:   runHarnessClient,
}

func init() {
	harnessClientCmd.Flags().StringVar(&harnessServerAddr, "server", "localhost:50053", "The server address for the gRPC HarnessService.")
	harnessClientCmd.Flags().StringVar(&harnessClientID, "harness", "testharness", "The harness id to send on the request envelope.")
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

	fmt.Print("Client > ")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	input := scanner.Text()

	stream, err := client.Connect(ctx)
	if err != nil {
		return fmt.Errorf("failed to open connection stream: %v", err)
	}

	// A single HarnessRequest{start} initiates the turn.
	start := &proto.HarnessRequest{
		ConversationId: uuid.NewString(),
		HarnessId:      harnessClientID,
		Type: &proto.HarnessRequest_Start{
			Start: &proto.HarnessStart{
				Messages: []*proto.Message{
					{
						Role: "user",
						Content: &proto.Content{
							Type: &proto.Content_Text{Text: &proto.TextContent{Text: input}},
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
			for i, m := range payload.Outputs.Messages {
				var text string
				if tb, ok := m.Content.Type.(*proto.Content_Text); ok {
					text = tb.Text.Text
				}
				fmt.Printf("Server > message[%d] (%s): %s\n", i, m.Role, text)
			}
		case *proto.HarnessResponse_End:
			fmt.Printf("Server > [end] state=%s %s\n", payload.End.GetState(), payload.End.GetErrorMessage())
		}
	}

	log.Println("Stream closed successfully by server.")
	return nil
}
