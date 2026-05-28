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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/google/ax/proto"
)

type mockHandler struct {
	mu       sync.Mutex
	messages []*proto.Message
	complete bool
	err      error
}

func (h *mockHandler) OnMessage(ctx context.Context, execID string, msg *proto.Message) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, msg)
	return h.err
}

func (h *mockHandler) OnComplete(ctx context.Context, execID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.complete = true
	return nil
}

func TestAntigravityHarness_Run_Success(t *testing.T) {
	upgrader := websocket.Upgrader{}

	// Spin up a local mock WebSocket server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("failed to upgrade connection: %v", err)
			return
		}
		defer conn.Close()

		// 1. Read initial start message
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("failed to read start message: %v", err)
			return
		}

		var payload map[string]any
		if err := json.Unmarshal(msg, &payload); err != nil {
			t.Errorf("failed to unmarshal start payload: %v", err)
			return
		}

		if payload["conversation_id"] != "conv-test" {
			t.Errorf("expected conversation ID 'conv-test', got %v", payload["conversation_id"])
		}

		// 2. Stream response chunks
		chunks := []map[string]any{
			{"type": "thought", "content": "Analyzing request"},
			{"type": "tool_call", "id": "call-123", "name": "get_weather", "args": map[string]any{"city": "Paris"}},
			{"type": "text", "content": "The weather in Paris is rainy."},
			{"type": "complete"},
		}

		for _, chunk := range chunks {
			bytes, _ := json.Marshal(chunk)
			if err := conn.WriteMessage(websocket.TextMessage, bytes); err != nil {
				t.Errorf("failed to write chunk: %v", err)
				return
			}
		}
	}))
	defer server.Close()

	// Convert http:// to ws://
	wsURL := strings.Replace(server.URL, "http://", "ws://", 1) + "/ws"

	harnessClient := NewAntigravityHarness(wsURL)
	exec, err := harnessClient.Start(context.Background(), "conv-test")
	if err != nil {
		t.Fatalf("failed to start execution: %v", err)
	}
	defer exec.Close(context.Background())

	msg := &proto.Message{
		Role: "user",
		Content: &proto.Content{
			Type: &proto.Content_Text{Text: &proto.TextContent{Text: "Hi"}},
		},
	}
	if err := exec.Queue(context.Background(), msg); err != nil {
		t.Fatalf("failed to queue message: %v", err)
	}

	handler := &mockHandler{}
	err = exec.Run(context.Background(), handler)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	handler.mu.Lock()
	defer handler.mu.Unlock()

	if !handler.complete {
		t.Error("expected OnComplete to be called")
	}
	if len(handler.messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(handler.messages))
	}
	if handler.messages[0].GetContent().GetThought().GetSummary()[0].GetText().GetText() != "Analyzing request" {
		t.Errorf("expected 'Analyzing request', got %q", handler.messages[0].GetContent().GetThought().GetSummary()[0].GetText().GetText())
	}
	
	toolCall := handler.messages[1].GetContent().GetToolCall()
	if toolCall == nil {
		t.Fatal("expected tool call message, got nil")
	}
	if toolCall.Id != "call-123" {
		t.Errorf("expected ID 'call-123', got %q", toolCall.Id)
	}
	if toolCall.GetFunctionCall().Name != "get_weather" {
		t.Errorf("expected name 'get_weather', got %q", toolCall.GetFunctionCall().Name)
	}
	if toolCall.GetFunctionCall().Arguments.GetFields()["city"].GetStringValue() != "Paris" {
		t.Errorf("expected arg city='Paris', got %q", toolCall.GetFunctionCall().Arguments.GetFields()["city"].GetStringValue())
	}

	if handler.messages[2].GetContent().GetText().GetText() != "The weather in Paris is rainy." {
		t.Errorf("expected 'The weather in Paris is rainy.', got %q", handler.messages[2].GetContent().GetText().GetText())
	}
}

func TestAntigravityHarness_Run_ErrorFrame(t *testing.T) {
	upgrader := websocket.Upgrader{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		_, _, _ = conn.ReadMessage()

		errFrame := map[string]string{
			"type":  "error",
			"error": "internal model crash",
		}
		bytes, _ := json.Marshal(errFrame)
		conn.WriteMessage(websocket.TextMessage, bytes)
	}))
	defer server.Close()

	wsURL := strings.Replace(server.URL, "http://", "ws://", 1) + "/ws"

	harnessClient := NewAntigravityHarness(wsURL)
	exec, _ := harnessClient.Start(context.Background(), "conv-test")
	defer exec.Close(context.Background())

	handler := &mockHandler{}
	err := exec.Run(context.Background(), handler)
	if err == nil {
		t.Fatal("expected error from Run(), got nil")
	}
	if !strings.Contains(err.Error(), "antigravity harness server error: internal model crash") {
		t.Errorf("unexpected error message: %v", err)
	}
}
