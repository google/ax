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
package controller

import (
	"context"
	"fmt"
	"maps"

	"github.com/google/ax/internal/agent"
	"github.com/google/ax/internal/controller/executor"
	"github.com/google/ax/internal/gemini"
	"github.com/google/ax/proto"
	"github.com/google/uuid"
)

const (
	plannerAgentID = "__planner"
	geminiAgentID  = "gemini"
)

var reservedAgentIDs = map[string]struct{}{
	plannerAgentID: {},
	geminiAgentID:  {},
}

type ExecHandler func(resp *proto.ExecResponse) error

// Controller is the main controller that coordinates all components.
// It acts as a single-writer system for managing agentic loops.
type Controller struct {
	registry       *Registry
	eventLog       executor.EventLog
	plannerBuilder PlannerBuilder
}

// PlannerBuilder is a function that creates a PlanFunc given a Registry.
type PlannerBuilder func(ctx context.Context, r *Registry) (agent.Agent, error)

// Config configures the controller.
type Config struct {
	EventLogBuilder executor.EventLogBuilder
	PlannerBuilder  PlannerBuilder
}

// New creates a new controller instance.
func New(ctx context.Context, cfg Config) (*Controller, error) {
	// Initialize agent registry
	registry := NewRegistry()

	// Determine plan function
	// If no planner builder is provided, use the default Gemini planner.
	if cfg.PlannerBuilder == nil {
		cfg.PlannerBuilder = func(ctx context.Context, r *Registry) (agent.Agent, error) {
			return gemini.NewGeminiPlannerAgent(ctx, r, gemini.GeminiPlannerConfig{})
		}
	}

	if cfg.EventLogBuilder == nil {
		return nil, fmt.Errorf("event log builder is required")
	}
	eventLog, err := cfg.EventLogBuilder()
	if err != nil {
		return nil, fmt.Errorf("failed to create event log: %w", err)
	}

	return &Controller{
		registry:       registry,
		eventLog:       eventLog,
		plannerBuilder: cfg.PlannerBuilder,
	}, nil
}

func (d *Controller) tryResuming(ctx context.Context, req *proto.ExecRequest, el executor.EventLog, registry map[string]agent.Agent, handler ExecHandler) (history []*proto.Message, done bool, err error) {
	events, err := el.Events(ctx, req.ConversationId)
	if err != nil {
		return nil, false, fmt.Errorf("failed to retrieve execution history: %w", err)
	}
	var pendingExecID string
	for _, ev := range events {
		if ev.ExecId != "" && ev.State == proto.State_STATE_PENDING {
			pendingExecID = ev.ExecId
		}
		if ev.ExecId == pendingExecID && ev.State == proto.State_STATE_COMPLETED {
			pendingExecID = ""
		}
		history = append(history, ev.Messages...)
	}

	if req.LastSeq != 0 {
		found := false
		for _, ev := range events {
			if ev.Seq == req.LastSeq {
				found = true
			}
			if ev.Seq > req.LastSeq {
				if err := handler(&proto.ExecResponse{
					Outputs: ev.Messages,
					Seq:     ev.Seq,
				}); err != nil {
					return nil, false, err
				}
			}
		}
		if !found {
			return nil, false, fmt.Errorf("last_seq %d not found", req.LastSeq)
		}
	}

	if pendingExecID == "" {
		return history, false, nil
	}

	// Find the pending event.
	execEvents, err := el.ExecEvents(ctx, pendingExecID)
	if err != nil {
		return nil, false, fmt.Errorf("failed to retrieve execution events: %w", err)
	}

	// TODO(jbd): Merge ExecutionEvent and ConversationEvent?
	var pendingEvent *proto.ExecutionEvent
	for _, ev := range execEvents {
		if ev.State == proto.State_STATE_PENDING {
			pendingEvent = ev
			break
		}
	}
	if pendingEvent == nil {
		return nil, false, fmt.Errorf("failed to retrieve pending event: %w", err)
	}
	if err := d.execute(
		ctx,
		req.ConversationId,
		pendingExecID,
		pendingEvent.AgentId,
		pendingEvent.AgentConfig,
		history,
		req.Inputs,
		registry,
		handler,
	); err != nil {
		return nil, false, err
	}
	return history, true, nil
}

// Exec executes a new agentic loop execution or resumes an existing one.
// If id is empty, a UUID will be generated.
// If the execution already exists, it will be resumed with optional new inputs.
func (d *Controller) Exec(ctx context.Context, req *proto.ExecRequest, handler ExecHandler) error {
	if req.ConversationId == "" {
		return fmt.Errorf("conversation_id is required")
	}

	planner, err := d.plannerBuilder(ctx, d.registry)
	if err != nil {
		return fmt.Errorf("failed to create planner: %w", err)
	}

	registry := maps.Clone(d.registry.Map())
	registry[plannerAgentID] = planner

	// TODO(lhuan): consider remove this.
	registry[geminiAgentID] = gemini.NewGeminiAgent()

	if req.AgentId == "" {
		req.AgentId = plannerAgentID
	}

	// Replay the execution history if any.
	history, done, err := d.tryResuming(ctx, req, d.eventLog, registry, handler)
	if err != nil {
		return err
	}
	if done {
		// Nothing else to do, new inputs are used in the replay.
		return nil
	}

	return d.execute(
		ctx,
		req.ConversationId,
		uuid.NewString(),
		req.AgentId,
		req.AgentConfig,
		history,
		req.Inputs,
		registry,
		handler,
	)
}

func (d *Controller) execute(ctx context.Context, conversationID string, execID string, agentID string, agentConfig []byte, history []*proto.Message, newInputs []*proto.Message, registry map[string]agent.Agent, handler ExecHandler) error {
	e := executor.DefaultExecutor(d.eventLog, registry)
	outputCapturer := func(outgoing *proto.AgentOutputs) error {
		// Filter out internal-only messages.
		var outputs []*proto.Message
		for _, m := range outgoing.Messages {
			if m.GetInternalOnly() {
				continue
			}
			outputs = append(outputs, m)
		}
		if len(outputs) == 0 {
			return nil
		}
		msg := &proto.ConversationEvent{
			ConversationId: conversationID,
			ExecId:         execID,
			Messages:       outputs,
			State:          proto.State_STATE_PENDING,
		}
		seq, err := d.eventLog.Append(ctx, msg)
		if err != nil {
			return err
		}
		return handler(&proto.ExecResponse{
			Outputs: msg.Messages,
			Seq:     seq,
		})
	}
	if _, err := d.eventLog.Append(ctx, &proto.ConversationEvent{
		ConversationId: conversationID,
		ExecId:         execID,
		Messages:       newInputs,
		State:          proto.State_STATE_PENDING,
	}); err != nil {
		return err
	}
	state, err := e.Exec(ctx, conversationID, execID, &proto.AgentStart{
		AgentId:     agentID,
		AgentConfig: agentConfig,
		Messages:    append(history, newInputs...),
	}, outputCapturer)
	if err != nil {
		return err
	}
	_, err = d.eventLog.Append(ctx, &proto.ConversationEvent{
		ConversationId: conversationID,
		ExecId:         execID,
		State:          state,
	})
	return err
}

// Delete deletes all events for a specific conversation ID.
func (d *Controller) Delete(ctx context.Context, conversationID string) error {
	if conversationID == "" {
		return fmt.Errorf("conversation_id is required")
	}

	return d.eventLog.DeleteEvents(ctx, conversationID)
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
