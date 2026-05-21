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
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/google/ax/cmd/ax/internal"
	"github.com/google/ax/cmd/ax/internal/cliutil"
	"github.com/google/ax/internal/controller"
	"github.com/google/ax/proto"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	execConversationID string
	execAgentID        string
	execInput          string
	execServerAddr     string
	execConfigFile     string
	execResume         bool // allow resuming an execution without inputs
	execLastSeq        int32
)

var execCmd = &cobra.Command{
	Use:   "exec",
	Short: "Execute a conversation or resume an existing one",
	Long: `Execute a new conversation or resume an existing one.
If no conversation ID is provided, a new UUID will be generated.`,
	RunE: runExec,
}

func init() {
	execCmd.Flags().StringVar(&execConversationID, "conversation", "", "Conversation ID (optional, generates UUID if not provided)")
	execCmd.Flags().StringVar(&execAgentID, "agent", "", "Agent ID (optional, planner is used if not specified)")
	execCmd.Flags().StringVar(&execInput, "input", "", "Input message to send (optional)")
	execCmd.Flags().StringVar(&execServerAddr, "server", "", "gRPC controller server address (if specified, connects to remote server; otherwise runs with a local built-in AX server)")
	execCmd.Flags().StringVar(&execConfigFile, "config", "ax.yaml", "Path to YAML configuration file (only used with a local built-in AX server)")
	execCmd.Flags().BoolVar(&execResume, "resume", false, "Resume a conversation without inputs")
	execCmd.Flags().Int32Var(&execLastSeq, "last-seq", 0, "Last sequence number seen by the client")
	execCmd.MarkFlagsMutuallyExclusive("input", "resume")
}

// TODO(jbd): Add multimodal input flags, e.g. --input-image.

var (
	execController *controller.Controller
)

func runExec(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	if execConversationID == "" {
		execConversationID = uuid.NewString()
	}

	// Setup signal handling for graceful shutdown
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\nReceived interrupt, shutting down...")
		if execController != nil {
			execController.Close()
		}
		cancel()
	}()

	if execServerAddr == "" {
		cfg, err := newConfig(cmd, execConfigFile)
		if err != nil {
			return err
		}
		// Validate configuration (matches `ax serve`).
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("invalid configuration: %w", err)
		}
		c, err := cliutil.NewControllerFromConfig(ctx, cfg)
		if err != nil {
			return fmt.Errorf("error creating controller: %w", err)
		}
		execController = c
	}

	return execLoop(ctx, execConversationID, execAgentID, execInput, execLastSeq)
}

func execLoop(ctx context.Context, id string, agentID string, input string, lastSeq int32) error {
	d := internal.NewDisplay(id)
	d.DisplayHeader()

	var inputs []*proto.Message
	if !execResume {
		var quit bool
		var err error
		input, quit, err = promptUser(d, input)
		if err != nil {
			return err
		}
		if quit {
			return nil
		}
		inputs = []*proto.Message{
			{
				Role: "user",
				Content: &proto.Content{
					Type: &proto.Content_Text{
						Text: &proto.TextContent{
							Text: input,
						},
					},
				},
			},
		}
	}

	for {
		conf, err := runAutoExec(ctx, d, &proto.ExecRequest{
			ConversationId: id,
			AgentId:        agentID,
			Inputs:         inputs,
			LastSeq:        lastSeq,
		})
		lastSeq = 0 // disable resuming from sequence, user sees the seq on the screen
		if err != nil {
			return err
		}

		if conf != nil {
			for {
				approved, err := d.PromptForApproval(conf.Question)
				if err != nil {
					return err
				}
				var decision []*proto.Message
				if approved {
					decision = []*proto.Message{{
						Role: "user",
						Content: &proto.Content{
							Type: &proto.Content_Confirmation{
								Confirmation: &proto.ConfirmationContent{
									Id: conf.Id,
									Decision: &proto.ConfirmationContent_Approval{
										Approval: &proto.ApprovalDecision{Approved: true},
									},
								},
							},
						},
					}}
				} else {
					decision = []*proto.Message{{
						Role: "user",
						Content: &proto.Content{
							Type: &proto.Content_Confirmation{
								Confirmation: &proto.ConfirmationContent{
									Id: conf.Id,
									Decision: &proto.ConfirmationContent_Decline{
										Decline: &proto.DeclineDecision{Declined: true},
									},
								},
							},
						},
					}}
				}

				conf, err = runAutoExec(ctx, d, &proto.ExecRequest{
					ConversationId: id,
					AgentId:        agentID,
					Inputs:         decision,
				})
				if err != nil {
					return err
				}
				if conf == nil {
					break
				}
			}
		}

		var quit bool
		input, quit, err = promptUser(d, "")
		if err != nil {
			return err
		}
		if quit {
			return nil
		}

		inputs = []*proto.Message{
			{
				Role: "user",
				Content: &proto.Content{
					Type: &proto.Content_Text{
						Text: &proto.TextContent{
							Text: input,
						},
					},
				},
			},
		}
		agentID = "" // reset agent id for next turn
	}
}

func runAutoExec(ctx context.Context, d *internal.Display, req *proto.ExecRequest) (*proto.ConfirmationContent, error) {
	fn := runExecHeadless
	if execServerAddr != "" {
		fn = runExecServer
	}
	return fn(ctx, d, req)
}

func runExecHeadless(ctx context.Context, d *internal.Display, req *proto.ExecRequest) (*proto.ConfirmationContent, error) {
	var confirmation *proto.ConfirmationContent
	var lastSeq int32
	outputHandler := controller.ExecHandler(func(resp *proto.ExecResponse) error {
		for _, m := range resp.Outputs {
			if conf := m.GetContent().GetConfirmation(); conf != nil {
				confirmation = conf
			}
		}
		lastSeq = resp.Seq
		displayContents(d, resp.Outputs)
		return nil
	})
	if err := execController.Exec(ctx, req, outputHandler); err != nil {
		return nil, fmt.Errorf("error executing with local server: %w", err)
	}

	if confirmation == nil {
		d.FinishOutput(fmt.Sprintf("seq=%d", lastSeq))
	}
	return confirmation, nil
}

func runExecServer(ctx context.Context, d *internal.Display, req *proto.ExecRequest) (*proto.ConfirmationContent, error) {
	conn, err := connect(execServerAddr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	client := proto.NewControllerServiceClient(conn)
	stream, err := client.Exec(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("error executing: %w", err)
	}

	var confirmation *proto.ConfirmationContent
	var lastSeq int32
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("error receiving response: %w", err)
		}
		lastSeq = resp.Seq
		if resp.Outputs != nil {
			for _, m := range resp.Outputs {
				if conf := m.GetContent().GetConfirmation(); conf != nil {
					confirmation = conf
				}
			}
			displayContents(d, resp.Outputs)
		}
	}
	if confirmation == nil {
		d.FinishOutput(fmt.Sprintf("seq=%d", lastSeq))
	}
	return confirmation, nil
}

func displayContents(d *internal.Display, contents []*proto.Message) {
	for _, output := range contents {
		content := output.GetContent()
		if content == nil {
			continue
		}
		switch o := content.Type.(type) {
		case *proto.Content_Text:
			d.DisplayOutput(o.Text.Text)
		case *proto.Content_Confirmation:
			// Let the confirmation prompt handle displaying the question.
		case *proto.Content_ToolCall:
			// No-op for cleaner CLI logs
		case *proto.Content_ToolResult:
			// Only print if the tool returned an error, otherwise skip
			tr := o.ToolResult
			if fr := tr.GetFunctionResult(); fr != nil {
				if fr.GetResponse() != nil {
					respMap := fr.GetResponse().AsMap()
					if errStr, ok := respMap["error"]; ok {
						d.DisplayOutput(fmt.Sprintf("\n[TOOL ERROR for %s]\n%v\n", fr.Name, errStr))
					}
				}
			}
		case *proto.Content_Thought:
			for _, summary := range o.Thought.GetSummary() {
				if textContent := summary.GetText(); textContent != nil {
					d.DisplayOutput(fmt.Sprintf("Thinking: %s", textContent.Text))
				}
			}
		case *proto.Content_Image, *proto.Content_Audio, *proto.Content_Video, *proto.Content_Document:
			d.DisplayOutput(fmt.Sprintf("unsupported output type for display: %T", o))
		default:
			d.DisplayOutput(fmt.Sprintf("unknown output type: %v", o))
		}
	}
}

// promptUser loops until the user provides a non-empty input string.
// It returns:
//   - string: the valid user input
//   - bool: true if the user entered a quit command
//   - error: any error that occurred during prompting
func promptUser(d *internal.Display, input string) (string, bool, error) {
	for strings.TrimSpace(input) == "" {
		var err error
		input, err = d.PromptForInput()
		if err != nil {
			return "", false, err
		}
	}

	d.DisplayInput(input)
	if strings.ToLower(strings.TrimSpace(input)) == "q" {
		d.ShowResumption(execConversationID, execServerAddr)
		return "", true, nil
	}
	return input, false, nil
}
