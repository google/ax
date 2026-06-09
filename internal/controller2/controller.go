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

// Package controller implements the single-writer orchestrator that coordinates
// agentic loops, manages executions, and communicates with local and remote agents.
package controller2

import (
	"context"
	"fmt"
	"log"

	"github.com/google/ax/internal/controller/executor"
	"github.com/google/ax/internal/harness/harnesstest"
	"github.com/google/ax/proto"
	"github.com/google/uuid"
)

type ExecHandler func(resp *proto.ExecResponse) error

// Controller is the main controller that coordinates all components.
// It acts as a single-writer system for managing agentic loops.
type Controller struct {
	registry *Registry
	eventLog executor.EventLog
}

// Config configures the controller.
type Config struct {
	Registry        *Registry
	EventLogBuilder executor.EventLogBuilder
}

// New creates a new controller instance.
func New(ctx context.Context, cfg Config) (*Controller, error) {
	if cfg.Registry == nil {
		return nil, fmt.Errorf("registry is required")
	}
	if cfg.EventLogBuilder == nil {
		return nil, fmt.Errorf("event log builder is required")
	}
	eventLog, err := cfg.EventLogBuilder()
	if err != nil {
		return nil, fmt.Errorf("failed to create event log: %w", err)
	}

	return &Controller{
		registry: cfg.Registry,
		eventLog: eventLog,
	}, nil
}

// Exec executes a new agentic loop execution or resumes an existing one.
// If id is empty, a UUID will be generated.
// If the execution already exists, it will be resumed with optional new inputs.
func (d *Controller) Exec(ctx context.Context, req *proto.ExecRequest, handler ExecHandler) error {
	if req.ConversationId == "" {
		return fmt.Errorf("conversation_id is required")
	}

	// TODO(jbd): Resume an incomplete execution if there exists one.
	// TODO(jbd): Enable bringing a remote harness that implements HarnessService.
	// TODO(anj): We need to consolidate agents and harness registration.
	// Adding harness registration support temporarily.
	h, err := d.registry.Harness(req.AgentId)
	if err != nil {
		// Fallback to test harness
		log.Printf("WARNING: harness %s not found in registry, falling back to test harness: %v", req.AgentId, err)
		h = harnesstest.New()
	}
	exec, err := h.Start(ctx, req.ConversationId)
	if err != nil {
		return fmt.Errorf("failed to start harness session: %w", err)
	}
	defer exec.Close(ctx)
 
	if err := exec.Queue(ctx, req.Inputs...); err != nil {
		return fmt.Errorf("failed to queue inputs: %w", err)
	}

	// Log inputs before running harness
	inputEvent := &proto.ConversationEvent{
		ConversationId: req.ConversationId,
		ExecId:         exec.ID(),
		Messages:       req.Inputs,
		State:          proto.State_STATE_PENDING,
	}
	if _, err := d.eventLog.Append(ctx, inputEvent); err != nil {
		return fmt.Errorf("failed to log inputs: %w", err)
	}

	hhandler := &harnessHandler{
		conversationID: req.ConversationId,
		eventLog:       d.eventLog,
		execHandler:    handler,
	}
	if err := exec.Run(ctx, hhandler); err != nil {
		return fmt.Errorf("harness execution turn failed: %w", err)
	}

	return nil
}

type harnessHandler struct {
	conversationID string
	eventLog       executor.EventLog
	execHandler    ExecHandler
}

func (a *harnessHandler) OnMessage(ctx context.Context, execID string, msg *proto.Message) error {
	// Log every response received from the harness
	event := &proto.ConversationEvent{
		ConversationId: a.conversationID,
		ExecId:         execID,
		Messages:       []*proto.Message{msg},
		State:          proto.State_STATE_PENDING,
	}
	// TODO(anj): The harness should send the full input sent to get this particular response.
	if _, err := a.eventLog.Append(ctx, event); err != nil {
		log.Printf("WARNING: failed to log streamed message: %v", err)
	}

	if a.execHandler == nil {
		return nil
	}
	return a.execHandler(&proto.ExecResponse{
		Outputs: []*proto.Message{msg},
	})
}

func (a *harnessHandler) OnComplete(ctx context.Context, execID string) error {
	// Mark the execution turn as completed in the conversation log
	event := &proto.ConversationEvent{
		ConversationId: a.conversationID,
		ExecId:         execID,
		State:          proto.State_STATE_COMPLETED,
	}
	if _, err := a.eventLog.Append(ctx, event); err != nil {
		log.Printf("WARNING: failed to log completion event: %v", err)
	}
	return nil
}

// Delete deletes all events for a specific conversation ID.
func (d *Controller) Delete(ctx context.Context, conversationID string) error {
	if conversationID == "" {
		return fmt.Errorf("conversation_id is required")
	}

	return d.eventLog.DeleteEvents(ctx, conversationID)
}

// Fork forks an event log from a specific conversation up to a checkpoint.
func (d *Controller) Fork(ctx context.Context, srcConversationID string, srcSeq int32, destConversationID string) (string, error) {
	if srcConversationID == "" {
		return "", fmt.Errorf("src_conversation_id is required")
	}
	// TODO(anj-s): Check whether destination ID already exists and reject collisions.
	if destConversationID == "" {
		destConversationID = uuid.NewString()
	}

	events, err := d.eventLog.Events(ctx, srcConversationID)
	if err != nil {
		return "", fmt.Errorf("failed to retrieve source events: %w", err)
	}
	if len(events) == 0 {
		return "", fmt.Errorf("source conversation %s not found or has no events", srcConversationID)
	}

	// When the caller specifies srcSeq, require that it actually exists in
	// the source event log. Without this check a typo or stale checkpoint
	// silently degrades to "fork all events", which is misleading. Walk
	// the events once: stop as soon as we pass the requested seq, and
	// truncate the slice on an exact match so the copy loop below doesn't
	// need to re-check the bound.
	if srcSeq > 0 {
		found := false
		for i, ev := range events {
			if ev.Seq == srcSeq {
				events = events[:i+1]
				found = true
				break
			}
			if ev.Seq > srcSeq {
				break
			}
		}
		if !found {
			return "", fmt.Errorf("src_seq %d not found in conversation %s", srcSeq, srcConversationID)
		}
	}

	for _, ev := range events {
		// Clone the event to update the conversation ID.
		newEvent := &proto.ConversationEvent{
			ConversationId: destConversationID,
			Seq:            ev.Seq,
			ExecId:         ev.ExecId,
			Messages:       ev.Messages,
			State:          ev.State,
		}
		if _, err := d.eventLog.Append(ctx, newEvent); err != nil {
			return "", fmt.Errorf("failed to append forked event: %w", err)
		}
	}

	return destConversationID, nil
}

// Registry returns the agent registry.
func (d *Controller) Registry() *Registry {
	return d.registry
}

// Close gracefully shuts down the controller.
func (d *Controller) Close() error {
	if err := d.eventLog.Close(); err != nil {
		return fmt.Errorf("failed to close event log: %w", err)
	}
	if err := d.registry.Close(); err != nil {
		return fmt.Errorf("failed to close registry: %w", err)
	}
	return nil
}
