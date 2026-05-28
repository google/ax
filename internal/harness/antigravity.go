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

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/google/ax/proto"
	"github.com/google/uuid"
)

// Compile-time interface assertions.
var _ Harness = (*AntigravityHarness)(nil)
var _ Execution = (*antigravityExecution)(nil)

// AntigravityHarness implements the Harness interface by connecting to the
// Antigravity Python agent server over WebSockets.
type AntigravityHarness struct {
	address string
}

// NewAntigravityHarness creates a new AntigravityHarness with a configurable address.
func NewAntigravityHarness(address string) *AntigravityHarness {
	if address == "" {
		address = "ws://localhost:50053/ws"
	}
	return &AntigravityHarness{
		address: address,
	}
}

// Start implements Harness.Start.
func (h *AntigravityHarness) Start(ctx context.Context, conversationID string) (Execution, error) {
	return &antigravityExecution{
		harness:        h,
		conversationID: conversationID,
		id:             uuid.NewString(),
	}, nil
}

// antigravityExecution implements the Execution interface.
type antigravityExecution struct {
	harness        *AntigravityHarness
	conversationID string
	id             string

	mu     sync.Mutex
	queued []*proto.Message
	closed bool
}

// ID implements Execution.ID.
func (e *antigravityExecution) ID() string {
	return e.id
}

// Queue implements Execution.Queue.
func (e *antigravityExecution) Queue(ctx context.Context, msg ...*proto.Message) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return fmt.Errorf("execution is closed")
	}
	e.queued = append(e.queued, msg...)
	return nil
}

// Run implements Execution.Run.
// It connects to the Python server over WebSockets, sends the start payload containing history,
// and streams responses back to the handler.
func (e *antigravityExecution) Run(ctx context.Context, handler Handler) error {
	e.mu.Lock()
	inputs := e.queued
	e.queued = nil
	e.mu.Unlock()

	// 1. Establish WebSocket connection
	dialer := websocket.DefaultDialer
	conn, _, err := dialer.DialContext(ctx, e.harness.address, nil)
	if err != nil {
		return fmt.Errorf("failed to dial antigravity harness websocket at %s: %w", e.harness.address, err)
	}
	defer conn.Close()

	// 2. Serialize inputs using protojson to match Python Parse() requirements
	var serializedMessages []json.RawMessage
	for _, msg := range inputs {
		bytes, err := protojson.Marshal(msg)
		if err != nil {
			return fmt.Errorf("failed to marshal message to JSON: %w", err)
		}
		serializedMessages = append(serializedMessages, json.RawMessage(bytes))
	}

	// 3. Construct and send start payload
	startPayload := map[string]any{
		"conversation_id": e.conversationID,
		"exec_id":         e.id,
		"messages":        serializedMessages,
	}
	payloadBytes, err := json.Marshal(startPayload)
	if err != nil {
		return fmt.Errorf("failed to marshal start payload: %w", err)
	}

	err = conn.WriteMessage(websocket.TextMessage, payloadBytes)
	if err != nil {
		return fmt.Errorf("failed to send start payload over WebSocket: %w", err)
	}

	// 4. Stream responses from WebSocket
	type WSResponse struct {
		Type    string          `json:"type"`
		Content string          `json:"content"`
		Error   string          `json:"error"`
		ID      string          `json:"id"`
		Name    string          `json:"name"`
		Args    json.RawMessage `json:"args"`
	}

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("failed to read message from WebSocket: %w", err)
		}

		var resp WSResponse
		if err := json.Unmarshal(message, &resp); err != nil {
			return fmt.Errorf("failed to unmarshal WebSocket response: %w", err)
		}

		switch resp.Type {
		case "text":
			msg := &proto.Message{
				Role: "assistant",
				Content: &proto.Content{
					Type: &proto.Content_Text{
						Text: &proto.TextContent{Text: resp.Content},
					},
				},
			}
			if err := handler.OnMessage(ctx, e.id, msg); err != nil {
				return fmt.Errorf("failed to send message to handler: %w", err)
			}
		case "thought":
			msg := &proto.Message{
				Role: "model",
				Content: &proto.Content{
					Type: &proto.Content_Thought{
						Thought: &proto.ThoughtContent{
							Summary: []*proto.ThoughtSummaryContent{
								{
									Type: &proto.ThoughtSummaryContent_Text{
										Text: &proto.TextContent{Text: resp.Content},
									},
								},
							},
						},
					},
				},
			}
			if err := handler.OnMessage(ctx, e.id, msg); err != nil {
				return fmt.Errorf("failed to send thought to handler: %w", err)
			}
		case "tool_call":
			var argsMap map[string]any
			if len(resp.Args) > 0 {
				if err := json.Unmarshal(resp.Args, &argsMap); err != nil {
					return fmt.Errorf("failed to unmarshal tool call args: %w", err)
				}
			}
			structArgs, err := structpb.NewStruct(argsMap)
			if err != nil {
				return fmt.Errorf("failed to create structpb from tool call args: %w", err)
			}

			msg := &proto.Message{
				Role: "model",
				Content: &proto.Content{
					Type: &proto.Content_ToolCall{
						ToolCall: &proto.ToolCallContent{
							Id: resp.ID,
							Type: &proto.ToolCallContent_FunctionCall{
								FunctionCall: &proto.FunctionCallContent{
									Name:      resp.Name,
									Arguments: structArgs,
								},
							},
						},
					},
				},
			}
			if err := handler.OnMessage(ctx, e.id, msg); err != nil {
				return fmt.Errorf("failed to send tool call to handler: %w", err)
			}
		case "complete":
			return handler.OnComplete(ctx, e.id)
		case "error":
			return fmt.Errorf("antigravity harness server error: %s", resp.Error)
		default:
			return fmt.Errorf("unknown response type from WebSocket: %q", resp.Type)
		}
	}
}

// Close implements Execution.Close.
func (e *antigravityExecution) Close(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	return nil
}
