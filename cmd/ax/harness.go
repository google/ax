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
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/ax/proto"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
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

// HarnessServiceServer implements the gRPC proto.HarnessServiceServer interface.
type HarnessServiceServer struct {
	proto.UnimplementedHarnessServiceServer
}

// NewHarnessServiceServer creates a new HarnessServiceServer.
func NewHarnessServiceServer() *HarnessServiceServer {
	return &HarnessServiceServer{}
}

// Connect implements the bidirectional gRPC streaming capability.
// It receives client inputs and responds only with "Hello world".
func (s *HarnessServiceServer) Connect(stream proto.HarnessService_ConnectServer) error {
	// TODO: Connect will be implemented to serve the built in harnesses
	// as an isolated actor.
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		err = stream.Send(&proto.HarnessMessage{
			Messages: []*proto.Message{
				{
					Role: "assistant",
					Content: &proto.Content{
						Type: &proto.Content_Text{
							Text: &proto.TextContent{
								Text: "Hello world",
							},
						},
					},
				},
			},
		})
		if err != nil {
			return err
		}
	}
}
