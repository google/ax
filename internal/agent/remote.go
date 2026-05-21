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

package agent

import (
	"context"
	"fmt"
	"io"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/google/ax/proto"
)

// RemoteAgent is a gRPC client that implements the Agent interface.
// It communicates with remote agent services over gRPC.
type RemoteAgent struct {
	cfg RemoteAgentConfig
}

// RemoteAgentConfig configures a remote agent client.
type RemoteAgentConfig struct {
	Address    string            // gRPC server address (e.g., "localhost:50051")
	Reconnect  bool              // Whether to automatically reconnect on failures
	MaxRetries int               // Maximum number of retry attempts (0 = infinite)
	DialOpts   []grpc.DialOption // gRPC dial options for customizing the connection
}

// NewRemoteAgent creates a new remote agent client.
func NewRemoteAgent(config RemoteAgentConfig) (*RemoteAgent, error) {
	if config.Address == "" {
		return nil, fmt.Errorf("agent address cannot be empty")
	}
	return &RemoteAgent{cfg: config}, nil
}

// connect establishes a gRPC connection to the remote agent.
func (a *RemoteAgent) connect() (*grpc.ClientConn, error) {
	// Use provided dial options, or default to insecure credentials
	opts := a.cfg.DialOpts
	if len(opts) == 0 {
		opts = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}

	conn, err := grpc.NewClient(a.cfg.Address, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to dial: %w", err)
	}
	return conn, nil
}

// Connect handles processing of input content with the remote agent.
func (a *RemoteAgent) Connect(ctx context.Context, conversationID string, execID string, start *proto.AgentStart, e Executor, o OutputHandler) error {
	ctx = metadata.AppendToOutgoingContext(ctx, "execution-id", execID)

	conn, err := a.connect()
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}
	defer conn.Close()

	client := proto.NewAgentServiceClient(conn)
	stream, err := client.Connect(ctx, &proto.AgentRequest{
		ConversationId: conversationID,
		ExecId:         execID,
		Start:          start,
	})
	if err != nil {
		return fmt.Errorf("failed to create stream: %w", err)
	}

	// Receive outputs and call handler for each
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			// Stream completed successfully
			return nil
		}
		if err != nil {
			return fmt.Errorf("failed to receive content: %w", err)
		}

		switch msg := resp.Type.(type) {
		case *proto.AgentResponse_Outputs:
			if resp.ConversationId != conversationID {
				return fmt.Errorf("received outputs for different conversation id: %s != %s", resp.ConversationId, conversationID)
			}
			if resp.ExecId != execID {
				return fmt.Errorf("received outputs for different execution id: %s != %s", resp.ExecId, execID)
			}
			if err := o(msg.Outputs); err != nil {
				return fmt.Errorf("handler error: %w", err)
			}
		case *proto.AgentResponse_End:
			if resp.ConversationId != conversationID {
				return fmt.Errorf("received end for different conversation id: %s != %s", resp.ConversationId, conversationID)
			}
			if resp.ExecId != execID {
				return fmt.Errorf("received end for different execution id: %s != %s", resp.ExecId, execID)
			}
			return nil
		default:
			return fmt.Errorf("unknown message type: %T", msg)
		}
	}
}

// Close gracefully shuts down the remote agent connection.
func (a *RemoteAgent) Close() error {
	return nil
}
