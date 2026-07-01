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
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/ax/cmd/ax/internal"
	"github.com/google/ax/cmd/ax/internal/cliutil"
	"github.com/google/ax/proto"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	execConversationID    string
	execHarnessID         string
	execHarnessConfig     string
	execHarnessConfigJSON string
	execInput             string
	execServerAddr        string
	execConfigFile        string
	execResume            bool // allow resuming an execution without inputs
	execLastSeq           int32
)

var execCmd = &cobra.Command{
	Use:   "exec",
	Short: "Execute a conversation or resume an existing one",
	Long: `Execute a new conversation or resume an existing one.
If no conversation ID is provided, a new UUID will be generated.`,
	SilenceUsage: true,
	RunE:         runExec,
}

func init() {
	execCmd.Flags().StringVar(&execConversationID, "conversation", "", "Conversation ID (optional, generates UUID if not provided)")
	execCmd.Flags().StringVar(&execHarnessID, "harness", "", "Harness ID (optional, default harness is used if not specified)")
	execCmd.Flags().StringVar(&execHarnessConfig, "harness-config", "", "Path to a JSON file with per-request harness configuration")
	execCmd.Flags().StringVar(&execHarnessConfigJSON, "harness-config-json", "", "Per-request harness configuration as an inline JSON string (mutually exclusive with --harness-config)")
	execCmd.Flags().StringVar(&execInput, "input", "", "Input message to send (optional)")
	execCmd.Flags().StringVar(&execServerAddr, "server", "", "gRPC controller server address (if specified, connects to remote server; otherwise runs with a local built-in AX server)")
	execCmd.Flags().StringVar(&execConfigFile, "config", "ax.yaml", "Path to YAML configuration file (only used with a local built-in AX server)")
	execCmd.Flags().BoolVar(&execResume, "resume", false, "Resume a conversation without inputs")
	execCmd.Flags().Int32Var(&execLastSeq, "last-seq", 0, "Last sequence number seen by the client")
	execCmd.MarkFlagsMutuallyExclusive("input", "resume")
	execCmd.MarkFlagsMutuallyExclusive("harness-config", "harness-config-json")
}

// TODO(jbd): Add multimodal input flags, e.g. --input-image.

var (
	// The concrete type is *controller.Controller
	execController   cliutil.Controller
	interruptHandler = NewInterruptHandler()
)

func runExec(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	if execConversationID == "" {
		execConversationID = uuid.NewString()
	}

	// Setup signal handling for graceful shutdown
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigChan := make(chan os.Signal, 2) // Buffer to not miss signals
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		for {
			sig := <-sigChan
			if sig == syscall.SIGTERM {
				fmt.Println("\nReceived SIGTERM, exiting...")
				interruptHandler.exit()
			}

			if !interruptHandler.TriggerCancel() {
				if interruptHandler.HandleInterrupt() {
					fmt.Println("\nExiting...")
					interruptHandler.exit()
				}
			}
		}
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

	var harnessConfig []byte
	if execHarnessConfig != "" {
		b, err := os.ReadFile(execHarnessConfig)
		if err != nil {
			return fmt.Errorf("failed to read harness config %q: %w", execHarnessConfig, err)
		}
		harnessConfig = b
	} else if execHarnessConfigJSON != "" {
		harnessConfig = []byte(execHarnessConfigJSON)
	}

	return execLoop(ctx, execConversationID, execHarnessID, harnessConfig, execInput, execLastSeq)
}

func execLoop(ctx context.Context, id string, harnessID string, harnessConfig []byte, input string, lastSeq int32) error {
	d := internal.NewDisplay(id, os.Stdout)
	d.DisplayHeader()

	var inputs []*proto.Message
	if !execResume {
		var quit bool
		var err error
		input, harnessConfig, quit, err = promptUser(d, input, harnessConfig)
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
		reqCtx, cancel := context.WithCancel(ctx)
		interruptHandler.SetActiveCancel(cancel)

		conf, err := runAutoExec(reqCtx, d, &proto.ExecRequest{
			ConversationId: id,
			HarnessId:      harnessID,
			HarnessConfig:  harnessConfig,
			Inputs:         inputs,
			LastSeq:        lastSeq,
		})
		lastSeq = 0 // disable resuming from sequence, user sees the seq on the screen

		interruptHandler.ClearActiveCancel()
		cancel()

		if err != nil {
			if errors.Is(err, context.Canceled) {
				fmt.Println("Request canceled.")
				inputs = nil
				continue
			}
			return err
		}

		if conf != nil {
			for {
				approved, err := d.PromptForApproval(conf.Question)
				if err != nil {
					if errors.Is(err, internal.ErrUserAborted) {
						if interruptHandler.HandleInterrupt() {
							return nil
						}
						continue
					}
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

				reqCtx, cancel := context.WithCancel(ctx)
				interruptHandler.SetActiveCancel(cancel)

				conf, err = runAutoExec(reqCtx, d, &proto.ExecRequest{
					ConversationId: id,
					HarnessId:      harnessID,
					HarnessConfig:  harnessConfig,
					Inputs:         decision,
				})

				interruptHandler.ClearActiveCancel()
				cancel()

				if err != nil {
					if errors.Is(err, context.Canceled) {
						fmt.Println("Request canceled.")
						break
					}
					return err
				}
				if conf == nil {
					break
				}
			}
		}

		// Per-request config: clear the config after each turn.
		harnessConfig = nil

		var quit bool
		input, harnessConfig, quit, err = promptUser(d, "", harnessConfig)
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
	outputHandler := cliutil.ExecHandler(func(resp *proto.ExecResponse) error {
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
		if content := output.GetContent(); content != nil {
			d.Display(content)
		}
	}
}

// promptUser loops until the user provides a non-empty input string.
// The "/config" command opens the harness config menu.
// It returns:
//   - string: the valid user input
//   - []byte: the (possibly updated) harness config
//   - bool: true if the user entered a quit command
//   - error: any error that occurred during prompting
func promptUser(d *internal.Display, input string, harnessConfig []byte) (string, []byte, bool, error) {
	for {
		for strings.TrimSpace(input) == "" {
			var err error
			input, err = d.PromptForInput()
			if err != nil {
				if errors.Is(err, internal.ErrUserAborted) {
					if interruptHandler.HandleInterrupt() {
						return "", harnessConfig, true, nil
					}
					input = "" // Continue loop to prompt again
					continue
				}
				return "", harnessConfig, false, err
			}
		}

		trimmed := strings.TrimSpace(input)
		if trimmed == "/config" {
			cfg, err := runConfigMenu(d, harnessConfig)
			if err != nil {
				return "", harnessConfig, false, err
			}
			harnessConfig = cfg
			input = "" // Re-prompt after handling the config.
			continue
		}

		d.DisplayInput(input)
		if strings.ToLower(trimmed) == "q" {
			d.ShowResumption(execConversationID, execServerAddr)
			return "", harnessConfig, true, nil
		}
		return input, harnessConfig, false, nil
	}
}

// InterruptHandler encapsulates the cancellation and signal handling state.
type InterruptHandler struct {
	mu             sync.Mutex
	activeCancel   context.CancelFunc
	interruptCount int32
}

// NewInterruptHandler creates a new InterruptHandler.
func NewInterruptHandler() *InterruptHandler {
	return &InterruptHandler{}
}

// SetActiveCancel sets the active cancel function.
func (h *InterruptHandler) SetActiveCancel(cancel context.CancelFunc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.activeCancel = cancel
}

// ClearActiveCancel clears the active cancel function.
func (h *InterruptHandler) ClearActiveCancel() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.activeCancel = nil
}

// HandleInterrupt increments the interrupt count and returns true if the process should exit.
// It also starts a timer to reset the count.
func (h *InterruptHandler) HandleInterrupt() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.interruptCount++
	if h.interruptCount == 1 {
		fmt.Println("\nPress Ctrl+C again to exit.")
		go func() {
			time.Sleep(2 * time.Second)
			h.mu.Lock()
			h.interruptCount = 0
			h.mu.Unlock()
		}()
		return false
	}
	return true
}

// TriggerCancel triggers cancellation if there is an active cancel function.
// It returns true if it triggered cancellation, or false if there was no active cancellation.
func (h *InterruptHandler) TriggerCancel() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.activeCancel != nil {
		fmt.Println("\nCanceling current request...")
		h.activeCancel()
		return true
	}
	return false
}

// exit gracefully shuts down the controller (if any) and terminates the process.
func (h *InterruptHandler) exit() {
	if execController != nil {
		execController.Close()
	}
	os.Exit(1)
}
