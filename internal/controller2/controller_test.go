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

	"github.com/google/ax/internal/agent"
	"github.com/google/ax/internal/controller/executor"
	"github.com/google/ax/internal/controller/executor/executortest"
	"github.com/google/ax/proto"
)

type dummyAgent struct{}

func (a *dummyAgent) Connect(ctx context.Context, conversationID string, execID string, start *proto.AgentStart, e agent.Executor, o agent.OutputHandler) error {
	return nil
}

func (a *dummyAgent) Close() error { return nil }

func TestController2_ExecHelloWorld(t *testing.T) {
	ctx := context.Background()
	cid := "test-conversation-id"

	log := &executortest.MemoryEventLog{}
	c, err := New(ctx, Config{
		EventLogBuilder: func() (executor.EventLog, error) {
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
}
