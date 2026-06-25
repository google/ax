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
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/ax/internal/agent"
	"github.com/google/ax/proto"
	"github.com/google/uuid"
	"google.golang.org/genai"
	"google.golang.org/protobuf/types/known/structpb"
)

type contextKey string

const disallowConfirmationsKey contextKey = "disallowConfirmations"

// AgentRegistry defines the interface needed by the planner to discover agents.
type AgentRegistry interface {
	List() []string
	AgentInfo(id string) (*agent.AgentInfo, error)
}

// ContentGenerator abstracts Gemini content generation for testing.
type ContentGenerator interface {
	GenerateContent(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error)
}

// realContentGenerator implements ContentGenerator using the real genai.Client.
type realContentGenerator struct {
	client *genai.Client
}

func (g *realContentGenerator) GenerateContent(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	return g.client.Models.GenerateContent(ctx, model, contents, config)
}

// GeminiPlannerConfig configures the Gemini-based planner.
type GeminiPlannerConfig struct {
	GeminiConfig *GeminiConfig
	SkillsDir    string // Directory for discovering skills (optional)
}

// geminiPlannerAgent implements task.Agent using Gemini.
type geminiPlannerAgent struct {
	config     GeminiPlannerConfig
	client     ContentGenerator
	bashTool   Tool
	skillsTool Tool
	registry   AgentRegistry
}

// NewGeminiPlannerAgent creates a new Gemini-based agent.
func NewGeminiPlannerAgent(ctx context.Context, registry AgentRegistry, cfg GeminiPlannerConfig) (agent.Agent, error) {
	if cfg.GeminiConfig == nil {
		cfg.GeminiConfig = &GeminiConfig{}
	}
	if cfg.GeminiConfig.Timeout == 0 {
		cfg.GeminiConfig.Timeout = 30 * time.Second
	}
	if cfg.GeminiConfig.Model == "" {
		cfg.GeminiConfig.Model = os.Getenv("AX_GEMINI_MODEL")
		if cfg.GeminiConfig.Model == "" {
			cfg.GeminiConfig.Model = "gemini-3.5-flash"
		}
	}

	// Default system prompt
	if cfg.GeminiConfig.SystemPrompt == "" {
		cfg.GeminiConfig.SystemPrompt = `You are AX. You are the Primary Architect and Executor Agent in the system. Your goal is to solve the user's request as thoroughly and efficiently as possible.
You can answer your questions if there is no tool or function call you need to make.
	
All specialized subagents available to you are registered and provided directly as standard callable function tools. 
You don't have access to shell, don't use the bash tool.

Rules for Operation:
- **DELEGATE FIRST**: Review all of your active function tool descriptions! If a tool is specialized for a delegated action (like building Docker images or deploying to Kubernetes), immediately execute that tool function. Do NOT try to perform specialized tasks yourself via shell commands if a custom agent tool exists.
- **Clarification**: If the user's request is highly ambiguous, ask for clarification. Keep all communication crisp, direct, and focused exclusively on action.

Be concise, don't come up with extra steps unless the user asks for it.
When introducing yourself, simply reply: "I am AX, how can I help you?"
`
	}

	// Fail fast if no Gemini credentials are configured. We check the three
	// env vars the underlying genai SDK recognizes (see
	// google.golang.org/genai/client.go NewClient docs):
	//   - GEMINI_API_KEY: AI Studio API key.
	//   - GOOGLE_API_KEY: alternate name for the AI Studio API key; the SDK
	//     accepts both and gives this one precedence when both are set.
	//   - GOOGLE_GENAI_USE_VERTEXAI: switches the SDK to the Vertex AI
	//     backend, which uses Application Default Credentials instead of an
	//     API key.
	if os.Getenv("GEMINI_API_KEY") == "" && os.Getenv("GOOGLE_API_KEY") == "" && os.Getenv("GOOGLE_GENAI_USE_VERTEXAI") == "" {
		return nil, fmt.Errorf("no Gemini credentials configured: set either GEMINI_API_KEY (AI Studio) " +
			"or GOOGLE_GENAI_USE_VERTEXAI=True with GOOGLE_CLOUD_PROJECT and GOOGLE_CLOUD_LOCATION (Vertex AI) " +
			"on the ax serve process and restart it; " +
			"see https://github.com/google/ax#authentication")
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{})
	if err != nil {
		return nil, fmt.Errorf("failed to create Gemini client: %w", err)
	}

	// NewSkillsTool already converts skills.ErrNoSkills (empty dir)
	// into a NoopTool with a nil error, so any error here is real.
	skillsTool, err := NewSkillsTool(cfg.SkillsDir)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize skills tool: %w", err)
	}

	p := &geminiPlannerAgent{
		client:     &realContentGenerator{client: client},
		bashTool:   &BashTool{},
		skillsTool: skillsTool,
		registry:   registry,
		config:     cfg,
	}

	if sp := skillsTool.SystemPrompt(); sp != "" {
		p.config.GeminiConfig.SystemPrompt += "\n\n" + sp
	}
	return p, nil
}

func (p *geminiPlannerAgent) Connect(ctx context.Context, conversationID string, execID string, start *proto.AgentStart, e agent.Executor, handler agent.OutputHandler) error {
	return p.loop(ctx, conversationID, start, e, handler)
}

func (p *geminiPlannerAgent) Close() error {
	return nil
}

func (p *geminiPlannerAgent) loop(ctx context.Context, conversationID string, start *proto.AgentStart, e agent.Executor, handler agent.OutputHandler) (err error) {
	var outputs []*proto.Message
	var outputCapturer = func(resp *proto.AgentOutputs) error {
		outputs = append(outputs, resp.Messages...)
		return handler(resp)
	}

	for {
		nextAgentID, keepLooping, err := p.process(ctx, conversationID, start, e, outputCapturer)
		if err != nil {
			return err
		}
		if keepLooping {
			// Some function calls require multiple turns to complete,
			// e.g. the skill activation and running.
			// Allow the loop to continue without switching agents or user input.
			start.Messages = append(start.Messages, outputs...)
			outputs = nil
			continue
		}

		if nextAgentID == "" {
			// No agent to delegate, we are done.
			return nil
		}
		start = &proto.AgentStart{
			AgentId:  nextAgentID,
			Messages: append(start.Messages, outputs...),
		}
		outputs = nil
		state, err := e.Exec(ctx, conversationID, nextAgentID, start, outputCapturer)
		if err != nil {
			return err
		}
		start.Messages = append(start.Messages, outputs...)
		outputs = nil
		if state == proto.State_STATE_PENDING {
			return nil
		}

	}
}

func (p *geminiPlannerAgent) process(ctx context.Context, conversationID string, start *proto.AgentStart, e agent.Executor, handler agent.OutputHandler) (agentID string, keepLooping bool, err error) {
	tools, err := agentsToTools(p.registry)
	if err != nil {
		return "", false, fmt.Errorf("failed to convert agents to tools: %w", err)
	}

	inputs := start.Messages
	if fc, approved := p.handleConfirmationAnswer(inputs); fc != nil {
		if fc.Name == p.bashTool.Name() {
			return "", false, p.bashTool.HandleExecute(ctx, fc, approved, handler)
		}
		if fc.Name == "run_skill_script" {
			return "", false, p.skillsTool.HandleExecute(ctx, fc, approved, handler)
		}
	}

	contents := protoToContents(inputs)
	ctx, cancel := context.WithTimeout(ctx, p.config.GeminiConfig.Timeout)
	defer cancel()

	genCfg := &genai.GenerateContentConfig{
		Tools: tools,
		ToolConfig: &genai.ToolConfig{
			FunctionCallingConfig: &genai.FunctionCallingConfig{
				Mode: genai.FunctionCallingConfigModeAuto,
			},
		},
		SystemInstruction: genai.Text(p.config.GeminiConfig.SystemPrompt)[0],
		MaxOutputTokens:   p.config.GeminiConfig.MaxTokens,
		CandidateCount:    1,
	}
	// Temperature is *float32 in genai: leave nil to inherit the model's
	// default. We treat the zero value in our config the same way.
	if t := p.config.GeminiConfig.Temperature; t > 0 {
		genCfg.Temperature = &t
	}

	resp, err := p.client.GenerateContent(ctx, p.config.GeminiConfig.Model, contents, genCfg)

	if err != nil {
		return "", false, fmt.Errorf("failed to generate in planner: %w", err)
	}
	if len(resp.Candidates) == 0 {
		return "", false, fmt.Errorf("no candidates from Gemini in planner")
	}
	candidate := resp.Candidates[0]
	if candidate.Content == nil || candidate.Content.Parts == nil {
		if candidate.FinishReason == genai.FinishReasonStop {
			return "", false, nil // No more tasks
		}
		return "", false, fmt.Errorf("no content in candidates from Gemini (reason: %v)", candidate.FinishReason)
	}

	// Look for function calls in the response
	for _, part := range candidate.Content.Parts {
		if part == nil {
			continue
		}

		trimmed := strings.TrimSpace(part.Text)
		if trimmed != "" {
			if err := handler(&proto.AgentOutputs{
				Messages: []*proto.Message{{
					Role: "model",
					Content: &proto.Content{
						Type: &proto.Content_Text{
							Text: &proto.TextContent{Text: trimmed},
						},
					},
				}},
			}); err != nil {
				return "", false, err
			}
		}

		if fc := part.FunctionCall; fc != nil {
			// Preserve Gemini's function-call ID when the model assigns
			// one so it round-trips correctly in subsequent turns. Only
			// mint a UUID when the model leaves it empty.
			if fc.ID == "" {
				fc.ID = uuid.NewString()
			}
			switch fc.Name {
			case p.bashTool.Name():
				return "", false, p.bashTool.HandleCall(ctx, fc, handler)
			case "run_skill_script":
				return "", false, p.skillsTool.HandleCall(ctx, fc, handler)
			case "activate_skill":
				return "", true, p.skillsTool.HandleCall(ctx, fc, handler)
			default:
				if p.isAgent(fc.Name) {
					return "", true, p.handleSubagentCall(ctx, conversationID, fc, part.ThoughtSignature, start.Messages, e, handler)
				}
				return strings.ReplaceAll(fc.Name, "_", "-"), false, nil
			}
		}
	}
	return "", false, nil
}

func (p *geminiPlannerAgent) isAgent(name string) bool {
	mappedName := strings.ReplaceAll(name, "_", "-")
	for _, a := range p.registry.List() {
		if a == mappedName {
			return true
		}
	}
	return false
}

func (p *geminiPlannerAgent) handleSubagentCall(ctx context.Context, conversationID string, fc *genai.FunctionCall, signature []byte, history []*proto.Message, e agent.Executor, handler agent.OutputHandler) error {

	prompt, ok := fc.Args["prompt"].(string)
	if !ok {
		return fmt.Errorf("missing or invalid 'prompt' argument")
	}

	// Record the function call in the history.
	argsStruct, err := structpb.NewStruct(fc.Args)
	if err != nil {
		return err
	}
	if err := handler(&proto.AgentOutputs{
		Messages: []*proto.Message{
			{
				Role: "model",
				Content: &proto.Content{
					Type: &proto.Content_ToolCall{
						ToolCall: &proto.ToolCallContent{
							Id:        fc.ID,
							Signature: signature,
							Type: &proto.ToolCallContent_FunctionCall{
								FunctionCall: &proto.FunctionCallContent{
									Name:      fc.Name,
									Arguments: argsStruct,
								},
							},
						},
					},
				},
			},
		},
	}); err != nil {
		return err
	}

	mappedName := strings.ReplaceAll(fc.Name, "_", "-")
	var historyStr strings.Builder
	for _, msg := range history {
		historyStr.WriteString(fmt.Sprintf("%s: ", msg.Role))
		if msg.Content != nil {
			if txt := msg.Content.GetText(); txt != nil {
				historyStr.WriteString(txt.Text)
			}
		}
		historyStr.WriteString("\n")
	}

	subagentStart := &proto.AgentStart{
		AgentId: mappedName,
		Messages: []*proto.Message{
			{
				Role: "user",
				Content: &proto.Content{
					Type: &proto.Content_Text{
						Text: &proto.TextContent{Text: fmt.Sprintf("History Summary:\n%s\n\nPrompt:\n%s", historyStr.String(), prompt)},
					},
				},
			},
		},
	}

	var subagentOutputs []*proto.Message
	capturer := func(outgoing *proto.AgentOutputs) error {
		subagentOutputs = append(subagentOutputs, outgoing.Messages...)
		return nil
	}

	var execErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				execErr = fmt.Errorf("panic: %v\n%s", r, string(debug.Stack()))
			}
		}()
		// Disallow tool confirmations for subagents.
		// NOTE: We use context here for simplicity, but for robust resumption support
		// across process restarts, this might need to be persisted in AgentConfig
		// in the future if the controller does not preserve context on resume.
		subCtx := context.WithValue(ctx, disallowConfirmationsKey, true)
		_, execErr = e.Exec(subCtx, conversationID, mappedName, subagentStart, capturer)
	}()

	if execErr != nil {
		respStruct, _ := structpb.NewStruct(map[string]any{
			"result": "sub agent failed",
			"error":  execErr.Error(),
		})
		return handler(&proto.AgentOutputs{
			Messages: []*proto.Message{
				{
					Role: "user",
					Content: &proto.Content{
						Type: &proto.Content_ToolResult{
							ToolResult: &proto.ToolResultContent{
								CallId: fc.ID,
								Type: &proto.ToolResultContent_FunctionResult{
									FunctionResult: &proto.FunctionResultContent{
										Name: fc.Name,
										Result: &proto.FunctionResultContent_Response{
											Response: respStruct,
										},
									},
								},
							},
						},
					},
				},
			},
		})
	}

	var resultText strings.Builder
	for _, msg := range subagentOutputs {
		if msg.Content != nil {
			if txt := msg.Content.GetText(); txt != nil {
				resultText.WriteString(txt.Text)
				resultText.WriteString("\n")
			}
		}
	}

	respStruct, err := structpb.NewStruct(map[string]any{"result": resultText.String()})
	if err != nil {
		return err
	}

	return handler(&proto.AgentOutputs{
		Messages: []*proto.Message{
			{
				Role: "user",
				Content: &proto.Content{
					Type: &proto.Content_ToolResult{
						ToolResult: &proto.ToolResultContent{
							CallId: fc.ID,
							Type: &proto.ToolResultContent_FunctionResult{
								FunctionResult: &proto.FunctionResultContent{
									Name: fc.Name,
									Result: &proto.FunctionResultContent_Response{
										Response: respStruct,
									},
								},
							},
						},
					},
				},
			},
		},
	})
}

func (p *geminiPlannerAgent) handleConfirmationAnswer(inputs []*proto.Message) (*genai.FunctionCall, bool) {
	var conf *proto.ConfirmationContent
	var approved bool
	for _, input := range inputs {
		content := input.GetContent()
		if content.GetConfirmation() != nil && content.GetConfirmation().GetApproval() != nil {
			conf = content.GetConfirmation()
			approved = true
		}
		if content.GetConfirmation() != nil && content.GetConfirmation().GetDecline() != nil {
			conf = content.GetConfirmation()
			approved = false
		}
	}
	if conf == nil {
		return nil, false
	}

	var fc *genai.FunctionCall
	for _, input := range inputs {
		content := input.GetContent()
		tc := content.GetToolCall()
		if tc == nil || tc.GetFunctionCall() == nil {
			continue
		}
		if tc.Id == conf.Id {
			fn := tc.GetFunctionCall()
			fc = &genai.FunctionCall{
				ID:   conf.Id,
				Name: fn.Name,
				Args: fn.Arguments.AsMap(),
			}
			break
		}
	}

	if fc == nil {
		return nil, false
	}

	// Ensure that we don't have a response for the function call.
	// Otherwise, we will execute the function call forever.
	for _, input := range inputs {
		content := input.GetContent()
		tr := content.GetToolResult()
		if tr == nil || tr.GetFunctionResult() == nil {
			continue
		}
		if tr.CallId == fc.ID {
			// We executed this previously.
			// There is nothing more to execute.
			return nil, false
		}
	}
	return fc, approved
}

// agentsToTools converts registry agents to Gemini function declarations.
func agentsToTools(registry AgentRegistry, nativeTools ...Tool) ([]*genai.Tool, error) {
	agents := registry.List()

	var tools []*genai.Tool
	// TODO(lhuan): Check if agentsToTools returns an error or empty list and return a friendly "no agent available, try later" error.
	for _, id := range agents {
		info, err := registry.AgentInfo(id)
		if err != nil {
			continue // Skip agents we can't get info for
		}

		// Set safe Gemini tool identifier logic alongside proper schema parameters
		funcDecl := &genai.FunctionDeclaration{
			Name:        strings.ReplaceAll(id, "-", "_"),
			Description: fmt.Sprintf("%s, %s", info.Name, info.Description),
			Parameters: &genai.Schema{
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"history": {
						Type:        genai.TypeString,
						Description: "Summary of the conversation history so far.",
					},
					"prompt": {
						Type:        genai.TypeString,
						Description: "The last user prompt verbatim.",
					},
				},
				Required: []string{"history", "prompt"},
			},
		}
		tools = append(tools, &genai.Tool{
			FunctionDeclarations: []*genai.FunctionDeclaration{funcDecl},
		})
	}
	for _, nativeTool := range nativeTools {
		if nativeTool == nil {
			continue
		}
		tools = append(tools, nativeTool.FuncDecl()...)
	}
	return tools, nil
}

// protoToContents converts history to Gemini conversation format.
func protoToContents(inputs []*proto.Message) []*genai.Content {
	var contents []*genai.Content

	// Convert each message to Gemini format
	for _, msg := range inputs {
		// Skip internal messages.
		if msg.GetInternalOnly() {
			continue
		}
		role := msg.Role
		if role != "user" {
			role = "model"
		}

		content := msg.GetContent()
		if content == nil {
			continue
		}

		switch m := content.Type.(type) {
		case *proto.Content_Text:
			contents = append(contents, &genai.Content{
				Role: role,
				Parts: []*genai.Part{
					{
						Text: m.Text.Text,
					},
				},
			})
		case *proto.Content_Confirmation:
			// shouldn't be sent to Gemini
			switch m.Confirmation.Decision.(type) {
			case *proto.ConfirmationContent_Decline:
				// shouldn't be sent to Gemini
			case *proto.ConfirmationContent_Approval:
				// shouldn't be sent to Gemini
			}
		case *proto.Content_ToolCall:
			tc := m.ToolCall
			if fc := tc.GetFunctionCall(); fc != nil {
				contents = append(contents, &genai.Content{
					Role: "model",
					Parts: []*genai.Part{
						{
							ThoughtSignature: tc.Signature,
							FunctionCall: &genai.FunctionCall{
								ID:   tc.Id,
								Name: fc.Name,
								Args: fc.Arguments.AsMap(),
							},
						},
					},
				})
			}
		case *proto.Content_ToolResult:
			tr := m.ToolResult
			if fr := tr.GetFunctionResult(); fr != nil {
				var respMap map[string]any
				if fr.GetResponse() != nil {
					respMap = fr.GetResponse().AsMap()
				}
				contents = append(contents, &genai.Content{
					Role: "user",
					Parts: []*genai.Part{
						{
							FunctionResponse: &genai.FunctionResponse{
								ID:       tr.CallId,
								Name:     fr.Name,
								Response: respMap,
							},
						},
					},
				})
			}
		}
		// TODO(jbd): Handle other content types (e.g., images, files)
	}
	return contents
}
