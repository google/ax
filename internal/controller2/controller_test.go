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

package controller2

import (
	"context"
	"testing"

	"github.com/google/ax/internal/controller2/eventlog"
	"github.com/google/ax/internal/controller2/eventlog/eventlogtest"
	"github.com/google/ax/internal/harness"
	"github.com/google/ax/proto"
)

type fakeHarness struct{}

func (f *fakeHarness) Start(ctx context.Context, conversationID string) (harness.Execution, error) {
	return &fakeExecution{id: "fake-exec-id"}, nil
}

type fakeExecution struct {
	id     string
	queued []*proto.Message
}

func (f *fakeExecution) ID() string {
	return f.id
}

func (f *fakeExecution) Queue(ctx context.Context, msg ...*proto.Message) error {
	f.queued = append(f.queued, msg...)
	return nil
}

func (f *fakeExecution) Run(ctx context.Context, handler harness.Handler) error {
	msg := &proto.Message{
		Role: "assistant",
		Content: &proto.Content{
			Type: &proto.Content_Text{
				Text: &proto.TextContent{
					Text: "Hello world",
				},
			},
		},
	}
	if err := handler.OnMessage(ctx, f.id, msg); err != nil {
		return err
	}
	return handler.OnComplete(ctx, f.id)
}

func (f *fakeExecution) Close(ctx context.Context) error {
	return nil
}

func TestController2_ExecHelloWorld(t *testing.T) {
	ctx := context.Background()
	cid := "test-conversation-id"

	log := &eventlogtest.MemoryEventLog{}
	reg := NewRegistry()
	if err := reg.RegisterHarness("", &fakeHarness{}); err != nil {
		t.Fatal(err)
	}

	c, err := New(ctx, Config{
		Registry: reg,
		EventLogBuilder: func() (eventlog.EventLog, error) {
			return log, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var outputs []*proto.Message
	handler := ExecHandler(func(resp *proto.ExecResponse) error {
		outputs = append(outputs, resp.Outputs...)
		return nil
	})

	inputs := []*proto.Message{
		{
			Role: "user",
			Content: &proto.Content{
				Type: &proto.Content_Text{
					Text: &proto.TextContent{Text: "Trigger prompt"},
				},
			},
		},
	}

	err = c.Exec(ctx, &proto.ExecRequest{
		ConversationId: cid,
		Inputs:         inputs,
	}, handler)
	if err != nil {
		t.Fatalf("Controller2.Exec failed: %v", err)
	}

	if len(outputs) != 1 {
		t.Fatalf("expected exactly 1 output message, got %d", len(outputs))
	}

	gotText := outputs[0].GetContent().GetText().GetText()
	if gotText != "Hello world" {
		t.Errorf("expected 'Hello world' output text response, got %q", gotText)
	}

	// Verify that events were logged correctly in Conversation Log
	events, err := log.Events(ctx, cid)
	if err != nil {
		t.Fatalf("failed to retrieve logged events: %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("expected 3 logged events, got %d", len(events))
	}

	// 1. First event should be inputs
	if len(events[0].Messages) != 1 {
		t.Errorf("expected 1 message in first event, got %d", len(events[0].Messages))
	} else {
		gotInputText := events[0].Messages[0].GetContent().GetText().GetText()
		if gotInputText != "Trigger prompt" {
			t.Errorf("expected 'Trigger prompt' in logged input, got %q", gotInputText)
		}
	}
	if events[0].State != proto.State_STATE_PENDING {
		t.Errorf("expected first event state to be PENDING, got %v", events[0].State)
	}

	// 2. Second event should be output
	if len(events[1].Messages) != 1 {
		t.Errorf("expected 1 message in second event, got %d", len(events[1].Messages))
	} else {
		gotOutputText := events[1].Messages[0].GetContent().GetText().GetText()
		if gotOutputText != "Hello world" {
			t.Errorf("expected 'Hello world' in logged output, got %q", gotOutputText)
		}
	}
	if events[1].State != proto.State_STATE_PENDING {
		t.Errorf("expected second event state to be PENDING, got %v", events[1].State)
	}

	// 3. Third event should be completion
	if len(events[2].Messages) != 0 {
		t.Errorf("expected 0 messages in third event, got %d", len(events[2].Messages))
	}
	if events[2].State != proto.State_STATE_COMPLETED {
		t.Errorf("expected third event state to be COMPLETED, got %v", events[2].State)
	}

	// Verify that no Execution Events were logged (moving away from granular execution log)
	if len(log.AllExecEvents) != 0 {
		t.Errorf("expected 0 execution events in V2, got %d: %v", len(log.AllExecEvents), log.AllExecEvents)
	}
}

func TestController2_ExecWithAgentID(t *testing.T) {
	ctx := context.Background()
	cid := "test-conversation-id"

	log := &eventlogtest.MemoryEventLog{}
	reg := NewRegistry()
	if err := reg.RegisterHarness("my-agent", &fakeHarness{}); err != nil {
		t.Fatal(err)
	}

	c, err := New(ctx, Config{
		Registry: reg,
		EventLogBuilder: func() (eventlog.EventLog, error) {
			return log, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var outputs []*proto.Message
	handler := ExecHandler(func(resp *proto.ExecResponse) error {
		outputs = append(outputs, resp.Outputs...)
		return nil
	})

	inputs := []*proto.Message{
		{
			Role: "user",
			Content: &proto.Content{
				Type: &proto.Content_Text{
					Text: &proto.TextContent{Text: "Trigger prompt"},
				},
			},
		},
	}

	err = c.Exec(ctx, &proto.ExecRequest{
		ConversationId: cid,
		AgentId:        "my-agent",
		Inputs:         inputs,
	}, handler)
	if err != nil {
		t.Fatalf("Controller2.Exec failed: %v", err)
	}

	if len(outputs) != 1 {
		t.Fatalf("expected exactly 1 output message, got %d", len(outputs))
	}

	gotText := outputs[0].GetContent().GetText().GetText()
	if gotText != "Hello world" {
		t.Errorf("expected 'Hello world' output text response, got %q", gotText)
	}
}

func TestController2_ExecHarnessNotFound(t *testing.T) {
	ctx := context.Background()
	cid := "test-conversation-id"

	log := &eventlogtest.MemoryEventLog{}
	reg := NewRegistry() // Empty registry, will force error for any requested agent

	c, err := New(ctx, Config{
		Registry: reg,
		EventLogBuilder: func() (eventlog.EventLog, error) {
			return log, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	handler := ExecHandler(func(resp *proto.ExecResponse) error {
		return nil
	})

	inputs := []*proto.Message{
		{
			Role: "user",
			Content: &proto.Content{
				Type: &proto.Content_Text{
					Text: &proto.TextContent{Text: "Trigger prompt"},
				},
			},
		},
	}

	err = c.Exec(ctx, &proto.ExecRequest{
		ConversationId: cid,
		Inputs:         inputs,
		AgentId:        "antigravity",
	}, handler)
	if err == nil {
		t.Fatal("expected error requesting unregistered agent, got nil")
	}
}
