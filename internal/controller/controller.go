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
	"log/slog"

	"github.com/google/ax/internal/controller/eventlog"
	"github.com/google/ax/proto"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
)

type ExecHandler func(resp *proto.ExecResponse) error

// Controller is the main controller that coordinates all components.
// It acts as a single-writer system for managing agentic loops.
type Controller struct {
	registry *Registry
	eventLog eventlog.EventLog
}

// Config configures the controller.
type Config struct {
	Registry        *Registry
	EventLogBuilder eventlog.EventLogBuilder
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
	h, err := d.registry.Harness(req.HarnessId)
	if err != nil {
		return fmt.Errorf("failed to get harness %q: %w", req.HarnessId, err)
	}

	l := newLogger(d.eventLog, req.ConversationId, req.HarnessId)
	state, err := l.ResumptionState(ctx)
	if err != nil {
		return fmt.Errorf("failed to check resumption state: %w", err)
	}

	hhandler := &harnessHandler{
		logger:      l,
		execHandler: handler,
	}

	if state == proto.State_STATE_PENDING {
		// If the state is pending, first try to resume the
		// pending execution. If the state is COMPLETED or FAILED, start
		// a new execution.
		exec, err := h.Start(ctx, req.ConversationId, req.HarnessConfig)
		if err != nil {
			return fmt.Errorf("failed to start harness session: %w", err)
		}
		defer exec.Close(ctx)

		if err := exec.Run(ctx, hhandler); err != nil {
			return fmt.Errorf("harness execution failed: %w", err)
		}
	}

	if len(req.Inputs) == 0 {
		// No more inputs, just return.
		return nil
	}

	exec, err := h.Start(ctx, req.ConversationId, req.HarnessConfig)
	if err != nil {
		return fmt.Errorf("failed to start harness session: %w", err)
	}
	defer exec.Close(ctx)

	if err := exec.Queue(ctx, req.Inputs...); err != nil {
		return fmt.Errorf("failed to queue inputs: %w", err)
	}
	// Log inputs before running harness
	if _, err := l.LogInputs(ctx, req.Inputs, req.HarnessConfig); err != nil {
		return fmt.Errorf("failed to log inputs: %w", err)
	}
	if err := exec.Run(ctx, hhandler); err != nil {
		return fmt.Errorf("harness execution failed: %w", err)
	}
	return nil
}

type harnessHandler struct {
	logger      *logger
	execHandler ExecHandler
}

func (a *harnessHandler) OnMessage(ctx context.Context, execID string, msg *proto.Message) error {
	// Log every response received from the harness
	// TODO(anj): The harness should send the full input sent to get this particular response.
	seq, err := a.logger.LogOutputs(ctx, []*proto.Message{msg}, proto.State_STATE_PENDING)
	if err != nil {
		slog.WarnContext(ctx, "Failed to log streamed message to event log",
			slog.String("conversation_id", a.logger.conversationID),
			slog.Any("error", err),
		)
	}

	if a.execHandler == nil {
		return nil
	}
	return a.execHandler(&proto.ExecResponse{
		Outputs: []*proto.Message{msg},
		Seq:     seq,
	})
}

func (a *harnessHandler) OnComplete(ctx context.Context, execID string) error {
	// Mark the execution turn as completed in the conversation log
	if _, err := a.logger.LogOutputs(ctx, nil, proto.State_STATE_COMPLETED); err != nil {
		slog.WarnContext(ctx, "Failed to log completion event to event log",
			slog.String("conversation_id", a.logger.conversationID),
			slog.Any("error", err),
		)
	}
	return nil
}

// Delete deletes all events for a specific conversation ID.
func (d *Controller) Delete(ctx context.Context, conversationID string) error {
	if conversationID == "" {
		return fmt.Errorf("conversation_id is required")
	}

	return d.eventLog.DeleteAll(ctx, conversationID)
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

func newLogger(
	el eventlog.EventLog,
	conversationID string,
	harnessID string) *logger {
	return &logger{
		el:             el,
		conversationID: conversationID,
		harnessID:      harnessID,
	}
}

type logger struct {
	conversationID string
	execID         string
	el             eventlog.EventLog
	harnessID      string
}

func (l *logger) ResumptionState(ctx context.Context) (proto.State, error) {
	events, err := l.el.Events(ctx, l.conversationID)
	if err != nil {
		return proto.State_STATE_UNSPECIFIED, err
	}

	var state proto.State
	for _, ev := range events {
		if ev.HarnessId != "" && ev.HarnessId != l.harnessID {
			return proto.State_STATE_UNSPECIFIED, fmt.Errorf("resumption not allowed: harness ID changed from %s to %s", ev.HarnessId, l.harnessID)
		}
		if l.execID == "" || ev.ExecId == l.execID {
			if ev.State != proto.State_STATE_UNSPECIFIED {
				state = ev.State
			}
		}
	}
	return state, nil
}

func (l *logger) LogInputs(ctx context.Context, inputs []*proto.Message, harnessConfig []byte) (int32, error) {
	// Parse the harness config into a human-readable struct for logging.
	var cfg *structpb.Struct
	if len(harnessConfig) > 0 {
		cfg = &structpb.Struct{}
		if err := protojson.Unmarshal(harnessConfig, cfg); err != nil {
			slog.WarnContext(ctx, "Failed to parse harness config for logging",
				slog.String("conversation_id", l.conversationID),
				slog.Any("error", err),
			)
			cfg = nil
		}
	}
	ev := &proto.ConversationEvent{
		ConversationId: l.conversationID,
		ExecId:         l.execID,
		HarnessId:      l.harnessID,
		HarnessConfig:  cfg,
		Messages:       inputs,
		State:          proto.State_STATE_PENDING,
	}
	return l.el.Append(ctx, ev)
}

func (l *logger) LogOutputs(ctx context.Context, outputs []*proto.Message, state proto.State) (int32, error) {
	ev := &proto.ConversationEvent{
		ConversationId: l.conversationID,
		ExecId:         l.execID,
		Messages:       outputs,
		State:          state,
	}
	return l.el.Append(ctx, ev)
}
