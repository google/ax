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
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"

	"google.golang.org/genai"
	"google.golang.org/grpc"

	"github.com/google/ax/internal/skills"
	"github.com/google/ax/proto"
	"github.com/google/uuid"
)

const port = ":50052"

type server struct {
	proto.UnimplementedAgentServiceServer
	genaiClient    *genai.Client
	skillsExecutor *skills.Executor
}

func (s *server) Connect(req *proto.AgentRequest, stream grpc.ServerStreamingServer[proto.AgentResponse]) error {
	start := req.GetStart()
	if start == nil {
		return errors.New("missing start message")
	}

	contents := protoToContents(start.Messages)

	for {
		model := os.Getenv("AX_GEMINI_MODEL")
		if model == "" {
			model = "gemini-3.5-flash"
		}
		resp, err := s.genaiClient.Models.GenerateContent(stream.Context(), model, contents, &genai.GenerateContentConfig{
			SystemInstruction: genai.Text("You are DockerAgent, an agent specialized in writing Dockerfiles. Use the docker-writer skill to fulfill requests. Keep responses concise.")[0],
			Tools:             []*genai.Tool{skills.BuildTool(s.skillsExecutor.SkillNames())},
		})
		if err != nil {
			return fmt.Errorf("failed to generate content: %w", err)
		}

		if len(resp.Candidates) == 0 {
			return errors.New("no candidates from Gemini")
		}
		candidate := resp.Candidates[0]

		var keepLooping bool
		var functionCall *genai.FunctionCall
		var thoughtSignature []byte

		for _, part := range candidate.Content.Parts {
			if part.FunctionCall != nil {
				functionCall = part.FunctionCall
				thoughtSignature = part.ThoughtSignature
				break
			}
		}

		if functionCall != nil {
			if functionCall.ID == "" {
				functionCall.ID = uuid.NewString()
			}

			// Handle skill call
			result, err := s.skillsExecutor.HandleCall(stream.Context(), functionCall)
			if err != nil {
				return fmt.Errorf("failed to handle skill call: %w", err)
			}

			// Append function call and response to history
			contents = append(contents, &genai.Content{
				Role: "model",
				Parts: []*genai.Part{{
					FunctionCall:     functionCall,
					ThoughtSignature: thoughtSignature,
				}},
			})
			contents = append(contents, &genai.Content{
				Role: "user",
				Parts: []*genai.Part{{
					FunctionResponse: &genai.FunctionResponse{
						ID:       functionCall.ID,
						Name:     functionCall.Name,
						Response: map[string]any{"result": result.Stdout},
					},
				}},
			})

			if functionCall.Name == "activate_skill" {
				keepLooping = true
			}
		}

		// Send text output if any (regardless of keepLooping)
		for _, part := range candidate.Content.Parts {
			if part.Text != "" {
				responseMsg := &proto.Message{
					Role: "assistant",
					Content: &proto.Content{
						Type: &proto.Content_Text{
							Text: &proto.TextContent{Text: "[DockerAgent] " + part.Text},
						},
					},
				}
				if err := stream.Send(&proto.AgentResponse{
					ConversationId: req.ConversationId,
					ExecId:         req.ExecId,
					Type: &proto.AgentResponse_Outputs{
						Outputs: &proto.AgentOutputs{
							Messages: []*proto.Message{responseMsg},
						},
					},
				}); err != nil {
					return err
				}
			}
		}

		if !keepLooping {
			break
		}
	}

	fmt.Println("DockerAgent finished execution turn.")

	// Send AgentEnd
	return stream.Send(&proto.AgentResponse{
		ConversationId: req.ConversationId,
		ExecId:         req.ExecId,
		Type: &proto.AgentResponse_End{
			End: &proto.AgentEnd{},
		},
	})
}

// protoToContents converts history to Gemini conversation format.
func protoToContents(inputs []*proto.Message) []*genai.Content {
	var contents []*genai.Content
	for _, msg := range inputs {
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
	}
	return contents
}

func (s *server) HealthCheck(ctx context.Context, req *proto.HealthCheckRequest) (*proto.HealthCheckResponse, error) {
	return &proto.HealthCheckResponse{
		Healthy: true,
		Message: "DockerAgent is healthy",
	}, nil
}

func main() {
	ctx := context.Background()
	client, err := genai.NewClient(ctx, &genai.ClientConfig{})
	if err != nil {
		log.Fatalf("Failed to create GenAI client: %v", err)
	}

	// Create skills executor pointing to the local skills directory
	skillsExecutor, err := skills.NewExecutor("./examples/docker_agent/skills")
	if err != nil {
		log.Fatalf("Failed to create skills executor: %v", err)
	}

	fmt.Printf("Listening on port: %s\n", port)
	lis, err := net.Listen("tcp", port)
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	proto.RegisterAgentServiceServer(grpcServer, &server{genaiClient: client, skillsExecutor: skillsExecutor})

	fmt.Println("\nDockerAgent server is running...")
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("Failed to serve: %v", err)
	}
}
