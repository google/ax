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
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/google/ax/proto"
	"github.com/google/uuid"
)

// AntigravityHarness implements the Harness interface by running the
// Antigravity Python agent as a subprocess.
type AntigravityHarness struct {
	scriptPath string
}

// NewAntigravityHarness creates a new AntigravityHarness with a configurable script path.
func NewAntigravityHarness(scriptPath string) *AntigravityHarness {
	if scriptPath == "" {
		scriptPath = "examples/antigravity_agent/agent.py"
	}
	return &AntigravityHarness{
		scriptPath: scriptPath,
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
// It executes the Python agent as a one-shot subprocess turn.
//
// NOTE: This is a stateless, subprocess-based validation harness. Because a new
// subprocess is launched for every turn, it does not support persistent state
// or dynamic input streaming. We retrieve all queued inputs at the start of the
// turn, clear the queue, and pass only the last user prompt to the subprocess.
// Full bidirectional streaming input/output will be supported once we migrate to
// the gRPC HarnessService server as noted below.
func (e *antigravityExecution) Run(ctx context.Context, handler Handler) error {
	e.mu.Lock()
	inputs := e.queued
	e.queued = nil
	e.mu.Unlock()

	// Extract only the latest user message since the CLI script only accepts a single prompt argument.
	var prompt string
	for i := len(inputs) - 1; i >= 0; i-- {
		msg := inputs[i]
		if msg.Role == "user" {
			if textContent := msg.GetContent().GetText().GetText(); textContent != "" {
				prompt = textContent
				break
			}
		}
	}

	// TODO(anj): Upgrade this to a gRPC HarnessService server to support full bidirectional
	// input/output streaming and avoid subprocess invocation overhead.
	
	// Prepare the command
	args := []string{e.harness.scriptPath}
	if prompt != "" {
		args = append(args, prompt)
	}

	cmd := exec.CommandContext(ctx, "python3", args...)
	
	// Capture stdout and stderr
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Run the command
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to run antigravity agent (stderr: %s): %w", stderr.String(), err)
	}

	output := strings.TrimSpace(stdout.String())

	// Send the output back to the handler
	msg := &proto.Message{
		Role: "assistant",
		Content: &proto.Content{
			Type: &proto.Content_Text{
				Text: &proto.TextContent{
					Text: output,
				},
			},
		},
	}

	if err := handler.OnMessage(ctx, e.id, msg); err != nil {
		return fmt.Errorf("failed to send message to handler: %w", err)
	}

	return handler.OnComplete(ctx, e.id)
}

// Close implements Execution.Close.
func (e *antigravityExecution) Close(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	return nil
}
