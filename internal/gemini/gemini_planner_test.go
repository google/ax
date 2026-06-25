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

package gemini

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/ax/internal/agent"
	"github.com/google/ax/proto"
	"google.golang.org/genai"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestProtoToContents(t *testing.T) {
	inputs := []*proto.Message{
		{
			Role: "user",
			Content: &proto.Content{
				Type: &proto.Content_Text{
					Text: &proto.TextContent{Text: "hello"},
				},
			},
		},
		{
			Role: "model",
			Content: &proto.Content{
				Type: &proto.Content_ToolCall{
					ToolCall: &proto.ToolCallContent{
						Id: "call-123",
						Type: &proto.ToolCallContent_FunctionCall{
							FunctionCall: &proto.FunctionCallContent{
								Name: "test_tool",
								Arguments: &structpb.Struct{
									Fields: map[string]*structpb.Value{
										"arg1": structpb.NewStringValue("val1"),
									},
								},
							},
						},
					},
				},
			},
		},
	}

	contents := protoToContents(inputs)

	if len(contents) != 2 {
		t.Fatalf("expected 2 contents, got %d", len(contents))
	}

	if contents[0].Role != "user" {
		t.Errorf("expected role user, got %s", contents[0].Role)
	}
	if contents[0].Parts[0].Text != "hello" {
		t.Errorf("expected text hello, got %s", contents[0].Parts[0].Text)
	}

	if contents[1].Role != "model" {
		t.Errorf("expected role model, got %s", contents[1].Role)
	}
	fc := contents[1].Parts[0].FunctionCall
	if fc == nil {
		t.Fatal("expected function call")
	}
	if fc.Name != "test_tool" {
		t.Errorf("expected function name test_tool, got %s", fc.Name)
	}
}

func TestHandleConfirmationAnswer(t *testing.T) {
	inputs := []*proto.Message{
		{
			Role: "model",
			Content: &proto.Content{
				Type: &proto.Content_ToolCall{
					ToolCall: &proto.ToolCallContent{
						Id: "call-123",
						Type: &proto.ToolCallContent_FunctionCall{
							FunctionCall: &proto.FunctionCallContent{
								Name: "bash",
								Arguments: &structpb.Struct{
									Fields: map[string]*structpb.Value{
										"command": structpb.NewStringValue("ls"),
									},
								},
							},
						},
					},
				},
			},
		},
		{
			Role: "user",
			Content: &proto.Content{
				Type: &proto.Content_Confirmation{
					Confirmation: &proto.ConfirmationContent{
						Id:       "call-123",
						Decision: &proto.ConfirmationContent_Approval{Approval: &proto.ApprovalDecision{Approved: true}},
					},
				},
			},
		},
	}

	p := &geminiPlannerAgent{
		bashTool: &BashTool{},
	}

	fc, approved := p.handleConfirmationAnswer(inputs)

	if fc == nil {
		t.Fatal("expected function call")
	}
	if fc.ID != "call-123" {
		t.Errorf("expected ID call-123, got %s", fc.ID)
	}
	if !approved {
		t.Error("expected approved to be true")
	}
}

type mockContentGenerator struct {
	generateContentFunc func(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error)
}

func (m *mockContentGenerator) GenerateContent(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	if m.generateContentFunc != nil {
		return m.generateContentFunc(ctx, model, contents, config)
	}
	return nil, nil
}

type mockExecutor struct {
	execFunc func(ctx context.Context, conversationID string, execID string, start *proto.AgentStart, o agent.OutputHandler) (proto.State, error)
}

func (m *mockExecutor) Exec(ctx context.Context, conversationID string, execID string, start *proto.AgentStart, o agent.OutputHandler) (proto.State, error) {
	if m.execFunc != nil {
		return m.execFunc(ctx, conversationID, execID, start, o)
	}
	return proto.State_STATE_COMPLETED, nil
}

type mockAgentRegistry struct {
	listFunc    func() []string
	getInfoFunc func(id string) (*agent.AgentInfo, error)
}

func (m *mockAgentRegistry) List() []string {
	if m.listFunc != nil {
		return m.listFunc()
	}
	return nil
}

func (m *mockAgentRegistry) AgentInfo(id string) (*agent.AgentInfo, error) {
	if m.getInfoFunc != nil {
		return m.getInfoFunc(id)
	}
	return nil, nil
}

func TestAgentsToTools_Parameters(t *testing.T) {
	registry := &mockAgentRegistry{
		listFunc: func() []string {
			return []string{"test-agent"}
		},
		getInfoFunc: func(id string) (*agent.AgentInfo, error) {
			return &agent.AgentInfo{
				ID:          "test-agent",
				Name:        "Test Agent",
				Description: "A test agent",
			}, nil
		},
	}

	tools, err := agentsToTools(registry)
	if err != nil {
		t.Fatalf("agentsToTools failed: %v", err)
	}

	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	decl := tools[0].FunctionDeclarations[0]
	if decl.Name != "test_agent" {
		t.Errorf("expected name test_agent, got %s", decl.Name)
	}

	params := decl.Parameters
	if params == nil {
		t.Fatal("expected parameters")
	}

	if params.Type != genai.TypeObject {
		t.Errorf("expected type object, got %v", params.Type)
	}

	if _, ok := params.Properties["history"]; !ok {
		t.Error("expected 'history' property")
	}
	if _, ok := params.Properties["prompt"]; !ok {
		t.Error("expected 'prompt' property")
	}

	required := params.Required
	if len(required) != 2 {
		t.Errorf("expected 2 required properties, got %d", len(required))
	}

	foundHistory := false
	foundPrompt := false
	for _, r := range required {
		if r == "history" {
			foundHistory = true
		}
		if r == "prompt" {
			foundPrompt = true
		}
	}
	if !foundHistory {
		t.Error("expected 'history' to be required")
	}
	if !foundPrompt {
		t.Error("expected 'prompt' to be required")
	}
}

func TestHandleSubagentCall_Success(t *testing.T) {
	mockGen := &mockContentGenerator{
		generateContentFunc: func(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
			return &genai.GenerateContentResponse{
				Candidates: []*genai.Candidate{
					{
						Content: &genai.Content{
							Parts: []*genai.Part{
								{Text: "Subagent work summary."},
							},
						},
					},
				},
			}, nil
		},
	}

	mockExec := &mockExecutor{
		execFunc: func(ctx context.Context, conversationID string, execID string, start *proto.AgentStart, o agent.OutputHandler) (proto.State, error) {
			o(&proto.AgentOutputs{
				Messages: []*proto.Message{
					{
						Role: "model",
						Content: &proto.Content{
							Type: &proto.Content_Text{
								Text: &proto.TextContent{Text: "Subagent output step 1"},
							},
						},
					},
				},
			})
			return proto.State_STATE_COMPLETED, nil
		},
	}

	p := &geminiPlannerAgent{
		client: mockGen,
		config: GeminiPlannerConfig{
			GeminiConfig: &GeminiConfig{Model: "test-model"},
		},
	}

	fc := &genai.FunctionCall{
		Name: "test-subagent",
		Args: map[string]any{
			"history": "Previous history summary",
			"prompt":  "Current user prompt",
		},
	}

	var handlerCalled bool
	var handlerOutput *proto.AgentOutputs
	handler := func(outgoing *proto.AgentOutputs) error {
		handlerCalled = true
		handlerOutput = outgoing
		return nil
	}

	err := p.handleSubagentCall(context.Background(), "test-conv", fc, nil, nil, mockExec, handler)
	if err != nil {
		t.Fatalf("handleSubagentCall failed: %v", err)
	}

	if !handlerCalled {
		t.Fatal("expected handler to be called")
	}

	if len(handlerOutput.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(handlerOutput.Messages))
	}
	msg := handlerOutput.Messages[0]
	tr := msg.Content.GetToolResult()
	if tr == nil {
		t.Fatal("expected tool result")
	}
	fr := tr.GetFunctionResult()
	if fr == nil {
		t.Fatal("expected function result")
	}
	if fr.Name != "test-subagent" {
		t.Errorf("expected name test-subagent, got %s", fr.Name)
	}
	resp := fr.GetResponse()
	if resp == nil {
		t.Fatal("expected response")
	}
	resultVal := resp.Fields["result"].GetStringValue()
	if resultVal != "Subagent output step 1\n" {
		t.Errorf("expected 'Subagent output step 1\\n', got %s", resultVal)
	}
}

func TestHandleSubagentCall_MissingArgs(t *testing.T) {
	p := &geminiPlannerAgent{}
	fc := &genai.FunctionCall{
		Name: "test-subagent",
		Args: map[string]any{},
	}

	err := p.handleSubagentCall(context.Background(), "test-conv", fc, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error due to missing arguments")
	}
	if !strings.Contains(err.Error(), "missing or invalid 'prompt' argument") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestHandleSubagentCall_Failure(t *testing.T) {
	mockExec := &mockExecutor{
		execFunc: func(ctx context.Context, conversationID string, execID string, start *proto.AgentStart, o agent.OutputHandler) (proto.State, error) {
			return proto.State_STATE_UNSPECIFIED, fmt.Errorf("subagent execution failed")
		},
	}

	p := &geminiPlannerAgent{}
	fc := &genai.FunctionCall{
		Name: "test-subagent",
		Args: map[string]any{
			"history": "Previous history summary",
			"prompt":  "Current user prompt",
		},
	}

	var handlerOutput *proto.AgentOutputs
	handler := func(outgoing *proto.AgentOutputs) error {
		handlerOutput = outgoing
		return nil
	}

	err := p.handleSubagentCall(context.Background(), "test-conv", fc, nil, nil, mockExec, handler)
	if err != nil {
		t.Fatalf("handleSubagentCall failed: %v", err)
	}

	msg := handlerOutput.Messages[0]
	tr := msg.Content.GetToolResult()
	fr := tr.GetFunctionResult()
	resp := fr.GetResponse()
	resultVal := resp.Fields["result"].GetStringValue()
	if resultVal != "sub agent failed" {
		t.Errorf("expected 'sub agent failed', got %s", resultVal)
	}
	errVal := resp.Fields["error"].GetStringValue()
	if !strings.Contains(errVal, "subagent execution failed") {
		t.Errorf("expected error to contain 'subagent execution failed', got %s", errVal)
	}
}

func TestHandleSubagentCall_Panic(t *testing.T) {
	mockExec := &mockExecutor{
		execFunc: func(ctx context.Context, conversationID string, execID string, start *proto.AgentStart, o agent.OutputHandler) (proto.State, error) {
			panic("something went wrong")
		},
	}

	p := &geminiPlannerAgent{}
	fc := &genai.FunctionCall{
		Name: "test-subagent",
		Args: map[string]any{
			"history": "Previous history summary",
			"prompt":  "Current user prompt",
		},
	}

	var handlerOutput *proto.AgentOutputs
	handler := func(outgoing *proto.AgentOutputs) error {
		handlerOutput = outgoing
		return nil
	}

	err := p.handleSubagentCall(context.Background(), "test-conv", fc, nil, nil, mockExec, handler)
	if err != nil {
		t.Fatalf("handleSubagentCall failed: %v", err)
	}

	msg := handlerOutput.Messages[0]
	tr := msg.Content.GetToolResult()
	fr := tr.GetFunctionResult()
	resp := fr.GetResponse()
	resultVal := resp.Fields["result"].GetStringValue()
	if resultVal != "sub agent failed" {
		t.Errorf("expected 'sub agent failed', got %s", resultVal)
	}
	errVal := resp.Fields["error"].GetStringValue()
	if !strings.Contains(errVal, "panic: something went wrong") {
		t.Errorf("expected error to contain 'panic: something went wrong', got %s", errVal)
	}
	if !strings.Contains(errVal, "goroutine") {
		t.Errorf("expected stack trace in error, got %s", errVal)
	}
}

// TestNewGeminiPlannerAgent_NoSkillsPrompt verifies that the Gemini planner agent
// system prompt does not contain the available_skills block when skills are disabled.
func TestNewGeminiPlannerAgent_NoSkillsPrompt(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "mock-key")
	t.Setenv("SKILLS_DIR", "")

	registry := &mockAgentRegistry{}
	cfg := GeminiPlannerConfig{
		GeminiConfig: &GeminiConfig{
			SystemPrompt: "You are AX.",
		},
		SkillsDir: "",
	}

	agent, err := NewGeminiPlannerAgent(context.Background(), registry, cfg)
	if err != nil {
		t.Fatalf("NewGeminiPlannerAgent failed: %v", err)
	}

	p, ok := agent.(*geminiPlannerAgent)
	if !ok {
		t.Fatalf("expected agent to be *geminiPlannerAgent, got %T", agent)
	}

	prompt := p.config.GeminiConfig.SystemPrompt
	if strings.Contains(prompt, "<available_skills>") {
		t.Errorf("expected system prompt to not contain '<available_skills>', got: %s", prompt)
	}
}
