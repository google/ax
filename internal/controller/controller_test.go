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

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/google/ax/internal/agent"
	"github.com/google/ax/internal/config"
	"github.com/google/ax/internal/controller/executor"
	"github.com/google/ax/internal/controller/executor/executortest"
	"github.com/google/ax/proto"
)

type dummyAgent struct{}

func (a *dummyAgent) Connect(ctx context.Context, conversationID string, execID string, start *proto.AgentStart, e agent.Executor, o agent.OutputHandler) error {
	return nil
}

func (a *dummyAgent) Close() error { return nil }

func TestController_Exec_ResumptionAndIDGeneration(t *testing.T) {
	ctx := context.Background()
	cid := "test-conv"

	inputs := []*proto.Message{
		{
			Role: "user",
			Content: &proto.Content{
				Type: &proto.Content_Text{
					Text: &proto.TextContent{Text: "hello"},
				},
			},
		},
	}

	// Case 1: No history, new inputs. Should create a new execution with a UUID.
	log := &executortest.MemoryEventLog{}
	c, err := New(ctx, Config{
		EventLogBuilder: func() (executor.EventLog, error) {
			return log, nil
		},
		PlannerBuilder: func(ctx context.Context, r *Registry) (agent.Agent, error) {
			return &dummyAgent{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	err = c.Exec(ctx, &proto.ExecRequest{
		ConversationId: cid,
		Inputs:         inputs,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(log.AllEvents) == 0 {
		t.Fatal("expected events to be logged")
	}
	execID := log.AllEvents[0].ExecId
	if execID == "" || execID == cid {
		t.Fatalf("expected a new random execID, got %v", execID)
	}

	// Case 2: History exists, PENDING state, inputs empty. Should replay/resume.
	// Modify the event logged by logPending in Case 1 to use dummy-agent.
	log.AllEvents[len(log.AllEvents)-1].State = proto.State_STATE_PENDING

	err = c.Exec(ctx, &proto.ExecRequest{
		ConversationId: cid,
		Inputs:         []*proto.Message{},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Replay should have called `e.Exec` for that `execID`.
	lastEventID := log.AllEvents[len(log.AllEvents)-1].ExecId
	if lastEventID != execID {
		t.Fatalf("expected resumed execution ID %v, got %v", execID, lastEventID)
	}

	// Case 3: History exists, COMPLETED state, new inputs. Should create a NEW execution.
	for _, ev := range log.AllExecEvents {
		if ev.ExecId == execID {
			ev.State = proto.State_STATE_COMPLETED
		}
	}
	// Also populate messages in conversation log to simulate completion.
	log.AllEvents[len(log.AllEvents)-1].Messages = []*proto.Message{
		{Role: "user", Content: &proto.Content{Type: &proto.Content_Text{Text: &proto.TextContent{Text: "hello"}}}},
	}
	log.AllEvents[len(log.AllEvents)-1].State = proto.State_STATE_COMPLETED

	err = c.Exec(ctx, &proto.ExecRequest{
		ConversationId: cid,
		Inputs:         inputs,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	newExecID := log.AllEvents[len(log.AllEvents)-1].ExecId
	if newExecID == execID {
		t.Fatal("expected a NEW execution ID, but it was reused")
	}
}

func TestController_Exec_LastSeq_Empty(t *testing.T) {
	ctx := context.Background()
	cid := "test-conv-seq"

	log := &executortest.MemoryEventLog{}
	// Pre-populate history
	log.AllEvents = []*proto.ConversationEvent{
		{
			ConversationId: cid,
			Seq:            1,
			Messages: []*proto.Message{
				{Role: "user", Content: &proto.Content{Type: &proto.Content_Text{Text: &proto.TextContent{Text: "msg 1"}}}},
			},
			State: proto.State_STATE_COMPLETED,
		},
		{
			ConversationId: cid,
			Seq:            2,
			Messages: []*proto.Message{
				{Role: "assistant", Content: &proto.Content{Type: &proto.Content_Text{Text: &proto.TextContent{Text: "msg 2"}}}},
			},
			State: proto.State_STATE_COMPLETED,
		},
	}

	c, err := New(ctx, Config{
		EventLogBuilder: func() (executor.EventLog, error) {
			return log, nil
		},
		PlannerBuilder: func(ctx context.Context, r *Registry) (agent.Agent, error) {
			return &dummyAgent{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var msgs []*proto.Message
	handler := ExecHandler(func(resp *proto.ExecResponse) error {
		msgs = append(msgs, resp.Outputs...)
		return nil
	})

	err = c.Exec(ctx, &proto.ExecRequest{
		ConversationId: cid,
	}, handler)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages, got %d", len(msgs))
	}
}

func TestController_Exec_LastSeq(t *testing.T) {
	ctx := context.Background()
	cid := "test-conv-seq"

	log := &executortest.MemoryEventLog{}
	// Pre-populate history
	log.AllEvents = []*proto.ConversationEvent{
		{
			ConversationId: cid,
			Seq:            1,
			Messages: []*proto.Message{
				{Role: "user", Content: &proto.Content{Type: &proto.Content_Text{Text: &proto.TextContent{Text: "msg 1"}}}},
			},
			State: proto.State_STATE_COMPLETED,
		},
		{
			ConversationId: cid,
			Seq:            2,
			Messages: []*proto.Message{
				{Role: "assistant", Content: &proto.Content{Type: &proto.Content_Text{Text: &proto.TextContent{Text: "msg 2"}}}},
			},
			State: proto.State_STATE_COMPLETED,
		},
	}

	c, err := New(ctx, Config{
		EventLogBuilder: func() (executor.EventLog, error) {
			return log, nil
		},
		PlannerBuilder: func(ctx context.Context, r *Registry) (agent.Agent, error) {
			return &dummyAgent{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var msgs []*proto.Message
	handler := ExecHandler(func(resp *proto.ExecResponse) error {
		msgs = append(msgs, resp.Outputs...)
		return nil
	})

	err = c.Exec(ctx, &proto.ExecRequest{
		ConversationId: cid,
		LastSeq:        1,
	}, handler)
	if err != nil {
		t.Fatal(err)
	}

	// We expect to receive messages from Seq 2 (since LastSeq is 1).
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].GetContent().GetText().GetText() != "msg 2" {
		t.Fatalf("expected 'msg 2', got %v", msgs[0].GetContent().GetText().GetText())
	}
}

func TestController_Exec_LastSeq_NotFound(t *testing.T) {
	ctx := context.Background()
	cid := "test-conv-seq-not-found"

	log := &executortest.MemoryEventLog{}
	// Pre-populate history
	log.AllEvents = []*proto.ConversationEvent{
		{
			ConversationId: cid,
			Seq:            1,
			Messages: []*proto.Message{
				{Role: "user", Content: &proto.Content{Type: &proto.Content_Text{Text: &proto.TextContent{Text: "msg 1"}}}},
			},
			State: proto.State_STATE_COMPLETED,
		},
	}

	c, err := New(ctx, Config{
		EventLogBuilder: func() (executor.EventLog, error) {
			return log, nil
		},
		PlannerBuilder: func(ctx context.Context, r *Registry) (agent.Agent, error) {
			return &dummyAgent{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	handler := ExecHandler(func(resp *proto.ExecResponse) error {
		return nil
	})

	err = c.Exec(ctx, &proto.ExecRequest{
		ConversationId: cid,
		LastSeq:        99,
	}, handler)
	if err == nil {
		t.Fatal("expected error when LastSeq is not found, got nil")
	}
	if err.Error() != "last_seq 99 not found" {
		t.Fatalf("expected 'last_seq 99 not found', got %v", err)
	}
}

func TestController_Exec_WaitsForConfirmation(t *testing.T) {
	ctx := context.Background()
	cid := "test-conv-conf"
	execID := "test-exec-conf"

	log := &executortest.MemoryEventLog{}

	// 1. History has a pending execution.
	log.AllEvents = []*proto.ConversationEvent{
		{
			ConversationId: cid,
			ExecId:         execID,
			State:          proto.State_STATE_PENDING,
			Seq:            1,
		},
	}

	// 2. The execution history ends with a confirmation question.
	questionMsg := &proto.Message{
		Role: "assistant",
		Content: &proto.Content{
			Type: &proto.Content_Confirmation{
				Confirmation: &proto.ConfirmationContent{
					Question: "Are you sure?",
				},
			},
		},
	}

	log.AllExecEvents = []*proto.ExecutionEvent{
		{
			ExecId:  execID,
			AgentId: "__planner",
			State:   proto.State_STATE_PENDING,
			Outputs: []*proto.Message{
				questionMsg,
			},
		},
	}

	c, err := New(ctx, Config{
		EventLogBuilder: func() (executor.EventLog, error) {
			return log, nil
		},
		PlannerBuilder: func(ctx context.Context, r *Registry) (agent.Agent, error) {
			return &dummyAgent{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var msgs []*proto.Message
	handler := ExecHandler(func(resp *proto.ExecResponse) error {
		msgs = append(msgs, resp.Outputs...)
		return nil
	})

	// Call Exec without providing an answer.
	err = c.Exec(ctx, &proto.ExecRequest{
		ConversationId: cid,
	}, handler)
	if err != nil {
		t.Fatal(err)
	}

	// We expect to receive the confirmation question again.
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].GetContent().GetConfirmation().GetQuestion() != "Are you sure?" {
		t.Fatalf("expected 'Are you sure?', got %v", msgs[0].GetContent().GetConfirmation().GetQuestion())
	}
}

func TestController_Exec_InternalOnly(t *testing.T) {
	ctx := context.Background()
	cid := "test-conv-internal"

	log := &executortest.MemoryEventLog{}

	// Create an agent that emits one internal-only message and one regular message.
	a := &mockAgentFunc{
		connectFunc: func(ctx context.Context, conversationID string, execID string, start *proto.AgentStart, e agent.Executor, o agent.OutputHandler) error {
			// If we already have the public message in history, don't emit anything.
			for _, m := range start.Messages {
				if m.GetContent().GetText().GetText() == "public message" {
					return nil
				}
			}
			// Emit internal-only message
			if err := o(&proto.AgentOutputs{
				Messages: []*proto.Message{
					{
						Role:         "assistant",
						InternalOnly: true,
						Content:      &proto.Content{Type: &proto.Content_Text{Text: &proto.TextContent{Text: "internal message"}}},
					},
				},
			}); err != nil {
				return err
			}
			// Emit regular message
			return o(&proto.AgentOutputs{
				Messages: []*proto.Message{
					{Role: "assistant", Content: &proto.Content{Type: &proto.Content_Text{Text: &proto.TextContent{Text: "public message"}}}},
				},
			})
		},
	}

	c, err := New(ctx, Config{
		EventLogBuilder: func() (executor.EventLog, error) {
			return log, nil
		},
		PlannerBuilder: func(ctx context.Context, r *Registry) (agent.Agent, error) {
			return a, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var msgs []*proto.Message
	handler := ExecHandler(func(resp *proto.ExecResponse) error {
		msgs = append(msgs, resp.Outputs...)
		return nil
	})

	err = c.Exec(ctx, &proto.ExecRequest{
		ConversationId: cid,
		Inputs: []*proto.Message{
			{Role: "user", Content: &proto.Content{Type: &proto.Content_Text{Text: &proto.TextContent{Text: "hello"}}}},
		},
	}, handler)
	if err != nil {
		t.Fatal(err)
	}

	// Verify that ONLY the public message was emitted.
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message emitted, got %d", len(msgs))
	}
	if msgs[0].GetContent().GetText().GetText() != "public message" {
		t.Fatalf("expected 'public message', got %v", msgs[0].GetContent().GetText().GetText())
	}

	// Verify that internal messages are NOT stored in ConversationEvent.
	if len(log.AllEvents) != 3 {
		t.Fatalf("expected 3 events in log.AllEvents, got %d", len(log.AllEvents))
	}

	// Event 0: Inputs
	// Event 1: Public message
	if log.AllEvents[1].Messages[0].GetContent().GetText().GetText() != "public message" {
		t.Fatalf("expected 'public message' in log.AllEvents, got %v", log.AllEvents[1].Messages[0].GetContent().GetText().GetText())
	}

	// Verify that BOTH messages ARE stored in ExecutionEvent.
	if len(log.AllExecEvents) != 3 {
		t.Fatalf("expected 3 events in log.AllExecEvents, got %d", len(log.AllExecEvents))
	}

	// Event 1 in execEvents should contain both outputs.
	outputs := log.AllExecEvents[1].Outputs
	if len(outputs) != 2 {
		t.Fatalf("expected 2 outputs in execEvent, got %d", len(outputs))
	}
	if outputs[0].GetContent().GetText().GetText() != "internal message" {
		t.Fatalf("expected 'internal message' in execEvent, got %v", outputs[0].GetContent().GetText().GetText())
	}
	if outputs[1].GetContent().GetText().GetText() != "public message" {
		t.Fatalf("expected 'public message' in execEvent, got %v", outputs[1].GetContent().GetText().GetText())
	}

	// Test resumption with LastSeq
	var resumedMsgs []*proto.Message
	resumedHandler := ExecHandler(func(resp *proto.ExecResponse) error {
		resumedMsgs = append(resumedMsgs, resp.Outputs...)
		return nil
	})

	c2, err := New(ctx, Config{
		EventLogBuilder: func() (executor.EventLog, error) {
			return log, nil
		},
		PlannerBuilder: func(ctx context.Context, r *Registry) (agent.Agent, error) {
			return a, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()

	err = c2.Exec(ctx, &proto.ExecRequest{
		ConversationId: cid,
		LastSeq:        1,
	}, resumedHandler)
	if err != nil {
		t.Fatal(err)
	}

	if len(resumedMsgs) != 1 {
		t.Fatalf("expected 1 message resumed, got %d", len(resumedMsgs))
	}
	if resumedMsgs[0].GetContent().GetText().GetText() != "public message" {
		t.Fatalf("expected 'public message' resumed, got %v", resumedMsgs[0].GetContent().GetText().GetText())
	}
}

type mockAgentFunc struct {
	connectFunc func(ctx context.Context, conversationID string, execID string, start *proto.AgentStart, e agent.Executor, o agent.OutputHandler) error
}

func (m *mockAgentFunc) Connect(ctx context.Context, conversationID string, execID string, start *proto.AgentStart, e agent.Executor, o agent.OutputHandler) error {
	return m.connectFunc(ctx, conversationID, execID, start, e, o)
}

func (m *mockAgentFunc) Close() error { return nil }

// TestController_Exec_ExplicitAgent_SkipsPlannerBuilder verifies that when
// the caller specifies a concrete --agent (i.e. ExecRequest.AgentId is not
// empty and is not the planner sentinel), the controller does NOT invoke the
// planner builder. This is what allows `ax exec --agent <name>` to run with
// no Gemini credentials configured. See google/ax#135.
func TestController_Exec_ExplicitAgent_SkipsPlannerBuilder(t *testing.T) {
	ctx := context.Background()
	cid := "test-conv-explicit-agent"

	log := &executortest.MemoryEventLog{}

	var ranAgent bool
	customAgent := &mockAgentFunc{
		connectFunc: func(_ context.Context, _, _ string, _ *proto.AgentStart, _ agent.Executor, o agent.OutputHandler) error {
			ranAgent = true
			return o(&proto.AgentOutputs{
				Messages: []*proto.Message{{
					Role:    "assistant",
					Content: &proto.Content{Type: &proto.Content_Text{Text: &proto.TextContent{Text: "hi from custom"}}},
				}},
			})
		},
	}

	var plannerBuilderCalls int
	c, err := New(ctx, Config{
		EventLogBuilder: func() (executor.EventLog, error) {
			return log, nil
		},
		PlannerBuilder: func(_ context.Context, _ *Registry) (agent.Agent, error) {
			plannerBuilderCalls++
			return nil, errors.New("no Gemini credentials configured (test sentinel: planner builder must not be called when --agent is explicit)")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Registry().RegisterLocal(config.LocalAgentConfig{
		ID:    "custom",
		Name:  "Custom",
		Agent: customAgent,
	}); err != nil {
		t.Fatalf("RegisterLocal: %v", err)
	}

	var msgs []*proto.Message
	handler := ExecHandler(func(resp *proto.ExecResponse) error {
		msgs = append(msgs, resp.Outputs...)
		return nil
	})

	err = c.Exec(ctx, &proto.ExecRequest{
		ConversationId: cid,
		AgentId:        "custom",
		Inputs: []*proto.Message{
			{Role: "user", Content: &proto.Content{Type: &proto.Content_Text{Text: &proto.TextContent{Text: "hello"}}}},
		},
	}, handler)
	if err != nil {
		t.Fatalf("Exec returned unexpected error: %v", err)
	}
	if plannerBuilderCalls != 0 {
		t.Fatalf("plannerBuilder was called %d time(s); want 0 (it must not be built when --agent is explicit)", plannerBuilderCalls)
	}
	if !ranAgent {
		t.Fatal("custom agent was not run")
	}
	if len(msgs) != 1 || msgs[0].GetContent().GetText().GetText() != "hi from custom" {
		t.Fatalf("unexpected outputs: %v", msgs)
	}
}

// TestController_Exec_DefaultAgent_BuildsPlanner verifies that when no
// --agent is provided, the controller still builds the planner (the existing
// default behaviour is preserved).
func TestController_Exec_DefaultAgent_BuildsPlanner(t *testing.T) {
	ctx := context.Background()
	cid := "test-conv-default-agent"

	log := &executortest.MemoryEventLog{}
	var plannerBuilderCalls int
	c, err := New(ctx, Config{
		EventLogBuilder: func() (executor.EventLog, error) {
			return log, nil
		},
		PlannerBuilder: func(_ context.Context, _ *Registry) (agent.Agent, error) {
			plannerBuilderCalls++
			return &dummyAgent{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	err = c.Exec(ctx, &proto.ExecRequest{
		ConversationId: cid,
		Inputs: []*proto.Message{
			{Role: "user", Content: &proto.Content{Type: &proto.Content_Text{Text: &proto.TextContent{Text: "hello"}}}},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plannerBuilderCalls != 1 {
		t.Fatalf("plannerBuilder was called %d time(s); want 1 (planner is required when --agent is not provided)", plannerBuilderCalls)
	}
}

// TestController_Exec_ResumePlannerExec_BuildsPlanner verifies that when an
// in-flight pending execution was originally driven by the planner, resuming
// it builds the planner even if the new request did not specify --agent (and
// thus would otherwise default to the planner anyway). This guards against a
// regression where the lazy-build refactor would skip the build during
// resumption.
func TestController_Exec_ResumePlannerExec_BuildsPlanner(t *testing.T) {
	ctx := context.Background()
	cid := "test-conv-resume-planner"
	execID := "test-exec-resume-planner"

	log := &executortest.MemoryEventLog{
		AllEvents: []*proto.ConversationEvent{
			{
				ConversationId: cid,
				ExecId:         execID,
				State:          proto.State_STATE_PENDING,
				Seq:            1,
			},
		},
		AllExecEvents: []*proto.ExecutionEvent{
			{
				ExecId:  execID,
				AgentId: plannerAgentID,
				State:   proto.State_STATE_PENDING,
				Outputs: []*proto.Message{
					{
						Role: "assistant",
						Content: &proto.Content{
							Type: &proto.Content_Confirmation{
								Confirmation: &proto.ConfirmationContent{Question: "?"},
							},
						},
					},
				},
			},
		},
	}

	var plannerBuilderCalls int
	c, err := New(ctx, Config{
		EventLogBuilder: func() (executor.EventLog, error) { return log, nil },
		PlannerBuilder: func(_ context.Context, _ *Registry) (agent.Agent, error) {
			plannerBuilderCalls++
			return &dummyAgent{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Exec(ctx, &proto.ExecRequest{ConversationId: cid}, func(*proto.ExecResponse) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if plannerBuilderCalls != 1 {
		t.Fatalf("plannerBuilder was called %d time(s); want 1 (resuming a planner-driven exec must build the planner)", plannerBuilderCalls)
	}
}

// TestController_Exec_ResumeCustomAgentExec_SkipsPlanner verifies the
// symmetric case: when the pending execution was driven by an explicit
// custom agent, resuming it must NOT require a planner.
func TestController_Exec_ResumeCustomAgentExec_SkipsPlanner(t *testing.T) {
	ctx := context.Background()
	cid := "test-conv-resume-custom"
	execID := "test-exec-resume-custom"

	log := &executortest.MemoryEventLog{
		AllEvents: []*proto.ConversationEvent{
			{
				ConversationId: cid,
				ExecId:         execID,
				State:          proto.State_STATE_PENDING,
				Seq:            1,
			},
		},
		AllExecEvents: []*proto.ExecutionEvent{
			{
				ExecId:  execID,
				AgentId: "custom",
				State:   proto.State_STATE_PENDING,
				Outputs: []*proto.Message{
					{
						Role: "assistant",
						Content: &proto.Content{
							Type: &proto.Content_Confirmation{
								Confirmation: &proto.ConfirmationContent{Question: "?"},
							},
						},
					},
				},
			},
		},
	}

	customAgent := &mockAgentFunc{
		connectFunc: func(_ context.Context, _, _ string, _ *proto.AgentStart, _ agent.Executor, _ agent.OutputHandler) error {
			return nil
		},
	}

	var plannerBuilderCalls int
	c, err := New(ctx, Config{
		EventLogBuilder: func() (executor.EventLog, error) { return log, nil },
		PlannerBuilder: func(_ context.Context, _ *Registry) (agent.Agent, error) {
			plannerBuilderCalls++
			return nil, errors.New("planner builder must not be called when resuming a custom-agent exec")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Registry().RegisterLocal(config.LocalAgentConfig{
		ID:    "custom",
		Agent: customAgent,
	}); err != nil {
		t.Fatalf("RegisterLocal: %v", err)
	}

	if err := c.Exec(ctx, &proto.ExecRequest{ConversationId: cid, AgentId: "custom"}, func(*proto.ExecResponse) error { return nil }); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if plannerBuilderCalls != 0 {
		t.Fatalf("plannerBuilder was called %d time(s); want 0", plannerBuilderCalls)
	}
}
