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
	"io"

	"github.com/google/ax/proto"
)

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
