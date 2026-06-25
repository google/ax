//go:build integration

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

package gemini_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/ax/internal/agent"
	"github.com/google/ax/internal/gemini"
	"github.com/google/ax/proto"
)

func TestIntegrationGeminiPlanner(t *testing.T) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		t.Skip("GEMINI_API_KEY not set, skipping integration test")
	}

	ctx := context.Background()
	registry := &dummyAgentRegistry{}

	cfg := gemini.GeminiPlannerConfig{}

	planner, err := gemini.NewGeminiPlannerAgent(ctx, registry, cfg)
	if err != nil {
		t.Fatal(err)
	}

	start := &proto.AgentStart{
		AgentId: "__planner",
		Messages: []*proto.Message{
			{
				Role: "user",
				Content: &proto.Content{
					Type: &proto.Content_Text{
						Text: &proto.TextContent{Text: "Hello, who are you?"},
					},
				},
			},
		},
	}

	var outputs []*proto.Message
	handler := func(resp *proto.AgentOutputs) error {
		outputs = append(outputs, resp.Messages...)
		return nil
	}

	err = planner.Connect(ctx, "test-conv", "test-exec", start, &dummyExecutor{}, handler)
	if err != nil {
		t.Fatal(err)
	}

	if len(outputs) == 0 {
		t.Fatal("expected outputs from Gemini")
	}

	// Wait, the agent will say "I am Antigravity..." or similar, or "I am AX..." depending on the system prompt.
	response := outputs[0].GetContent().GetText().GetText()
	t.Logf("Gemini response: %v", response)
	expectedPart := "AX"
	if !strings.Contains(response, expectedPart) {
		t.Errorf("Expected response to contain %q, got %q", expectedPart, response)
	}
}

type dummyExecutor struct{}

func (e *dummyExecutor) Exec(ctx context.Context, conversationID string, execID string, start *proto.AgentStart, o agent.OutputHandler) (proto.State, error) {
	return proto.State_STATE_COMPLETED, nil
}

type dummyAgentRegistry struct{}

func (r *dummyAgentRegistry) List() []string {
	return nil
}

func (r *dummyAgentRegistry) AgentInfo(id string) (*agent.AgentInfo, error) {
	return nil, fmt.Errorf("agent not found: %s", id)
}
