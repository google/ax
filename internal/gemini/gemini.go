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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/google/ax/internal/agent"
	"github.com/google/ax/internal/skills"
	"github.com/google/ax/proto"
	"google.golang.org/genai"
	"google.golang.org/protobuf/types/known/structpb"
)

// GeminiAgent implements task.Agent using Gemini.
type GeminiAgent struct {
}

// NewGeminiAgent creates a new Gemini agent.
func NewGeminiAgent() *GeminiAgent {
	return &GeminiAgent{}
}

func (a *GeminiAgent) config(start *proto.AgentStart) (*GeminiConfig, error) {
	if len(start.AgentConfig) == 0 {
		return &GeminiConfig{
			Model:   "gemini-3.5-flash",
			Timeout: 30 * time.Second,
		}, nil
	}

	var cfg GeminiConfig
	if err := json.Unmarshal(start.AgentConfig, &cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal Gemini config: %w", err)
	}
	return &cfg, nil
}

func (a *GeminiAgent) Connect(ctx context.Context, conversationID string, execID string, start *proto.AgentStart, e agent.Executor, handler agent.OutputHandler) error {
	cfg, err := a.config(start)
	if err != nil {
		return err
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{})
	if err != nil {
		return fmt.Errorf("failed to create Gemini client: %w", err)
	}

	inputs := start.Messages
	contents := protoToContents(inputs)
	timeout := 30 * time.Second
	if cfg.Timeout != 0 {
		timeout = cfg.Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var systemPrompt *genai.Content
	if cfg.SystemPrompt != "" {
		systemPrompt = genai.Text(cfg.SystemPrompt)[0]
	}
	var tools []*genai.Tool
	for _, t := range cfg.Tools {
		switch t {
		case "google_search":
			tools = append(tools, &genai.Tool{GoogleSearch: &genai.GoogleSearch{}})
		case "url_context":
			tools = append(tools, &genai.Tool{URLContext: &genai.URLContext{}})
		case "code_execution":
			tools = append(tools, &genai.Tool{CodeExecution: &genai.ToolCodeExecution{}})
		case "google_maps":
			tools = append(tools, &genai.Tool{GoogleMaps: &genai.GoogleMaps{}})
		default:
			return fmt.Errorf("unsupported tool: %q", t)
		}
	}

	resp, err := client.Models.GenerateContent(ctx, cfg.Model, contents, &genai.GenerateContentConfig{
		SystemInstruction: systemPrompt,
		MaxOutputTokens:   cfg.MaxTokens,
		CandidateCount:    1,
		Tools:             tools,
	})
	if err != nil {
		return fmt.Errorf("failed to generate: %w", err)
	}

	if len(resp.Candidates) == 0 {
		return fmt.Errorf("no candidates from Gemini in planner")
	}
	candidate := resp.Candidates[0]
	if candidate.Content == nil || candidate.Content.Parts == nil {
		return fmt.Errorf("no content in candidates from Gemini")
	}

	respCount := 0
	for _, part := range candidate.Content.Parts {
		if part == nil {
			continue
		}
		if part.Text != "" {
			respCount++
			if err := handler(&proto.AgentOutputs{
				Messages: []*proto.Message{{
					Role: "model",
					Content: &proto.Content{
						Type: &proto.Content_Text{
							Text: &proto.TextContent{Text: part.Text},
						},
					},
				}},
			}); err != nil {
				return err
			}
		}
	}
	if respCount == 0 {
		return errors.New("no responses from Gemini")
	}
	return nil
}

func (a *GeminiAgent) Close() error {
	return nil
}

type Tool interface {
	Name() string
	FuncDecl() []*genai.Tool
	SystemPrompt() string
	HandleCall(ctx context.Context, fc *genai.FunctionCall, o agent.OutputHandler) error
	HandleExecute(ctx context.Context, fc *genai.FunctionCall, approved bool, o agent.OutputHandler) error
}

type BashTool struct{}

func (t *BashTool) Name() string {
	return "bash"
}

func (t *BashTool) SystemPrompt() string {
	return ""
}

func (t *BashTool) FuncDecl() []*genai.Tool {
	osInfo := fmt.Sprintf("User's Operating System: %s (%s)", runtime.GOOS, runtime.GOARCH)
	description := fmt.Sprintf("OS specific bash execution tool. %s. Generate commands appropriate for this OS. Never produce code, only use existing command line programs available in the system.", osInfo)

	return []*genai.Tool{{
		FunctionDeclarations: []*genai.FunctionDeclaration{
			{
				Name:        t.Name(),
				Description: description,
				Parameters: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"command": {
							Type:        genai.TypeString,
							Description: "The shell command to execute (e.g., 'ls -la' for Unix/macOS, 'dir' for Windows, 'cat file.txt', etc.)",
						},
					},
					Required: []string{"command"},
				},
			},
		},
	}}
}

func (t *BashTool) HandleCall(ctx context.Context, fc *genai.FunctionCall, o agent.OutputHandler) error {
	if disallow, _ := ctx.Value(disallowConfirmationsKey).(bool); disallow {
		return t.HandleExecute(ctx, fc, true, o)
	}
	command, _ := fc.Args["command"].(string)
	argsStruct, err := structpb.NewStruct(fc.Args)
	if err != nil {
		return err
	}
	return o(&proto.AgentOutputs{
		Messages: []*proto.Message{
			{
				Role: "model",
				Content: &proto.Content{
					Type: &proto.Content_ToolCall{
						ToolCall: &proto.ToolCallContent{
							Id: fc.ID,
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
			{
				Role: "model",
				Content: &proto.Content{
					Type: &proto.Content_Confirmation{
						Confirmation: &proto.ConfirmationContent{
							Id:       fc.ID,
							Question: fmt.Sprintf("Can I run %q?", command),
						},
					},
				},
			}},
	})
}

func (t *BashTool) HandleExecute(ctx context.Context, fc *genai.FunctionCall, approved bool, o agent.OutputHandler) error {
	if !approved {
		// Declined, nothing to do in terms of executing any commands.
		// But we still have to finish with a function response,
		// not to keep the previously log function call hanging forever.
		return o(&proto.AgentOutputs{
			Messages: []*proto.Message{
				{
					Role: "model",
					Content: &proto.Content{
						Type: &proto.Content_ToolResult{
							ToolResult: &proto.ToolResultContent{
								CallId: fc.ID,
								Type: &proto.ToolResultContent_FunctionResult{
									FunctionResult: &proto.FunctionResultContent{
										Name: fc.Name,
									},
								},
							},
						},
					},
				},
				{
					Role: "model",
					Content: &proto.Content{
						Type: &proto.Content_Text{
							Text: &proto.TextContent{
								Text: "Okay.",
							},
						},
					},
				},
			},
		})
	}

	output, err := execute(ctx, fc.Args)
	if err != nil {
		return err
	}
	respStruct, err := structpb.NewStruct(map[string]any{"result": output})
	if err != nil {
		return fmt.Errorf("failed to convert function response to structpb: %w", err)
	}
	return o(&proto.AgentOutputs{
		Messages: []*proto.Message{
			{
				Role: "model",
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
			{
				Role: "model",
				Content: &proto.Content{
					Type: &proto.Content_Text{
						Text: &proto.TextContent{
							Text: output,
						},
					},
				},
			},
		},
	})
}

type SkillsTool struct {
	executor *skills.Executor
}

func NewSkillsTool(dir string) (Tool, error) {
	resolvedDir := dir
	if resolvedDir == "" {
		resolvedDir = os.Getenv("SKILLS_DIR")
	}
	if resolvedDir == "" {
		// No skills directory explicitly configured; disable skills completely.
		return &NoopTool{}, nil
	}
	if _, err := os.Stat(resolvedDir); os.IsNotExist(err) {
		return &NoopTool{}, nil
	}

	executor, err := skills.NewExecutor(resolvedDir)
	if err != nil {
		// If no skills are found or the directory does not exist, fallback to a no-op tool
		// to avoid hard errors when skills are not configured or available.
		if errors.Is(err, skills.ErrNoSkills) || errors.Is(err, os.ErrNotExist) {
			return &NoopTool{}, nil
		}
		return nil, err
	}
	return &SkillsTool{executor: executor}, nil
}

func (t *SkillsTool) Name() string {
	return ""
}

func (t *SkillsTool) SystemPrompt() string {
	return t.executor.SystemPrompt()
}

func (t *SkillsTool) FuncDecl() []*genai.Tool {
	if !t.executor.HasSkills() {
		return []*genai.Tool{}
	}
	return []*genai.Tool{skills.BuildTool(t.executor.SkillNames())}
}

func (t *SkillsTool) HandleCall(ctx context.Context, fc *genai.FunctionCall, o agent.OutputHandler) error {
	if disallow, _ := ctx.Value(disallowConfirmationsKey).(bool); disallow {
		return t.HandleExecute(ctx, fc, true, o)
	}
	argsStruct, err := structpb.NewStruct(fc.Args)
	if err != nil {
		return err
	}

	if fc.Name == "activate_skill" {
		// Skill activation is always approved.
		return t.HandleExecute(ctx, fc, true, o)
	}

	if fc.Name == "run_skill_script" {
		skill, _ := fc.Args["skill"].(string)
		script, _ := fc.Args["script"].(string)
		question := fmt.Sprintf("Can I run script %q from skill %q?", script, skill)

		return o(&proto.AgentOutputs{
			Messages: []*proto.Message{
				{
					Role: "model",
					Content: &proto.Content{
						Type: &proto.Content_ToolCall{
							ToolCall: &proto.ToolCallContent{
								Id: fc.ID,
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
				{
					Role: "model",
					Content: &proto.Content{
						Type: &proto.Content_Confirmation{
							Confirmation: &proto.ConfirmationContent{
								Id:       fc.ID,
								Question: question,
							},
						},
					},
				}},
		})
	}
	return nil
}

func (t *SkillsTool) HandleExecute(ctx context.Context, fc *genai.FunctionCall, approved bool, o agent.OutputHandler) error {
	if fc.Name == "activate_skill" {
		var output string
		result, err := t.executor.HandleCall(ctx, fc)
		if err != nil {
			output = "Error: " + err.Error()
		} else {
			var parts []string
			if result.Stdout != "" {
				parts = append(parts, result.Stdout)
			}
			if result.Stderr != "" {
				parts = append(parts, "Stderr: "+result.Stderr)
			}
			output = strings.Join(parts, "\n")
		}

		respStruct, err := structpb.NewStruct(map[string]any{"result": output})
		if err != nil {
			return fmt.Errorf("failed to convert function response to structpb: %w", err)
		}

		return o(&proto.AgentOutputs{
			Messages: []*proto.Message{
				{
					Role: "model",
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

	if !approved {
		// Declined, nothing to do in terms of executing any commands.
		// But we still have to finish with a function response,
		// not to keep the previously log function call hanging forever.
		return o(&proto.AgentOutputs{
			Messages: []*proto.Message{
				{
					Role: "model",
					Content: &proto.Content{
						Type: &proto.Content_ToolResult{
							ToolResult: &proto.ToolResultContent{
								CallId: fc.ID,
								Type: &proto.ToolResultContent_FunctionResult{
									FunctionResult: &proto.FunctionResultContent{
										Name: fc.Name,
									},
								},
							},
						},
					},
				},
				{
					Role: "model",
					Content: &proto.Content{
						Type: &proto.Content_Text{
							Text: &proto.TextContent{
								Text: "Okay.",
							},
						},
					},
				},
			},
		})
	}

	var output string
	if fc.Name == "run_skill_script" || fc.Name == "activate_skill" {
		result, err := t.executor.HandleCall(ctx, fc)
		if err != nil {
			output = "Error: " + err.Error()
		} else {
			var parts []string
			if result.Stdout != "" {
				parts = append(parts, result.Stdout)
			}
			if result.Stderr != "" {
				parts = append(parts, "Stderr: "+result.Stderr)
			}
			output = strings.Join(parts, "\n")
		}
	}

	respStruct, err := structpb.NewStruct(map[string]any{"result": output})
	if err != nil {
		return fmt.Errorf("failed to convert function response to structpb: %w", err)
	}
	return o(&proto.AgentOutputs{
		Messages: []*proto.Message{
			{
				Role: "model",
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
			{
				Role: "model",
				Content: &proto.Content{
					Type: &proto.Content_Text{
						Text: &proto.TextContent{
							Text: output,
						},
					},
				},
			},
		},
	})
}

type NoopTool struct {
}

func (t *NoopTool) Name() string {
	return ""
}

func (t *NoopTool) SystemPrompt() string {
	return ""
}

func (t *NoopTool) FuncDecl() []*genai.Tool {
	return []*genai.Tool{}
}

func (t *NoopTool) HandleCall(ctx context.Context, fc *genai.FunctionCall, o agent.OutputHandler) error {
	return errors.New("cannot call noop tool")
}

func (t *NoopTool) HandleExecute(ctx context.Context, fc *genai.FunctionCall, approved bool, o agent.OutputHandler) error {
	return errors.New("cannot execute noop tool")
}

func execute(ctx context.Context, args map[string]any) (string, error) {
	command, ok := args["command"].(string)
	if !ok {
		return "", fmt.Errorf("command parameter missing or invalid")
	}

	// Execute the command. exec.CommandContext kills the process if ctx
	// is cancelled (e.g. the planner loop is shutting down or the gRPC
	// Exec stream is cancelled), so a long-running command like
	// `find / -name ...` doesn't outlive the controller.
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		// Return both the error and any output that was produced
		return fmt.Sprintf("Error: %v\nOutput: %s\n\n", err, output), nil
	}

	result := strings.TrimSpace(string(output))
	if result == "" {
		return "Command executed successfully (no output)", nil
	}
	return result, nil
}
