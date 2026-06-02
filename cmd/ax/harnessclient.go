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
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/google/ax/proto"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	harnessServerAddr string
)

var harnessClientCmd = &cobra.Command{
	Use:    "harnessclient",
	Short:  "Run the harness client to connect to the server",
	Hidden: true,
	RunE:   runHarnessClient,
}

func init() {
	harnessClientCmd.Flags().StringVar(&harnessServerAddr, "server", "localhost:50053", "The server address for the gRPC HarnessService.")
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
		return fmt.Errorf("Failed to open connection stream: %v", err)
	}

	scanner := bufio.NewScanner(os.Stdin)
	fmt.Println("Interactive client started. Type your messages below.")
	fmt.Println("Type 'go_away' to close the stream and exit.")
	for {
		fmt.Print("\nClient > ")
		if !scanner.Scan() {
			break
		}
		text := scanner.Text()
		if text == "" {
			continue
		}

		msg := &proto.HarnessMessage{
			Messages: []*proto.Message{
				{
					Role: "user",
					Content: &proto.Content{
						Type: &proto.Content_Text{
							Text: &proto.TextContent{
								Text: text,
							},
						},
					},
				},
			},
		}

		if err := stream.Send(msg); err != nil {
			return fmt.Errorf("Failed to send message: %v", err)
		}
		// TODO(params): Replace this with a proper protocol for go away.
		if text == "go_away" {
			log.Println("Sending 'go_away' to close the stream...")
			break
		}

		resp, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("Failed to receive response: %v", err)
		}

		for i, m := range resp.Messages {
			var textContent string
			if textBlock, ok := m.Content.Type.(*proto.Content_Text); ok {
				textContent = textBlock.Text.Text
			}
			fmt.Printf("Server > message[%d] (%s): %s\n", i, m.Role, textContent)
		}
	}

	// Close send side to signal request completion
	if err := stream.CloseSend(); err != nil {
		return fmt.Errorf("Failed to close send side of stream: %v", err)
	}

	log.Println("Waiting for final stream EOF...")
	_, err = stream.Recv()
	if err != io.EOF {
		return fmt.Errorf("Expected EOF from server, got: %v", err)
	}
	log.Println("Stream closed successfully by server.")
	return nil
}
