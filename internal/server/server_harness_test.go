//go:build harness

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

package server

import (
	"context"
	"testing"

	"github.com/google/ax/internal/controller/executor"
	"github.com/google/ax/internal/controller/executor/executortest"
	"github.com/google/ax/internal/controller2"
	"github.com/google/ax/proto"
)

func TestServer_Fork(t *testing.T) {
	ctx := context.Background()
	srcCID := "src-conv"
	destCID := "dest-conv"

	log := &executortest.MemoryEventLog{}
	log.AllEvents = []*proto.ConversationEvent{
		{
			ConversationId: srcCID,
			Seq:            1,
			Messages: []*proto.Message{
				{Role: "user", Content: &proto.Content{Type: &proto.Content_Text{Text: &proto.TextContent{Text: "msg 1"}}}},
			},
			State: proto.State_STATE_COMPLETED,
		},
	}

	c, err := controller2.New(ctx, controller2.Config{
		EventLogBuilder: func() (executor.EventLog, error) {
			return log, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s := New(c)

	resp, err := s.ForkConversation(ctx, &proto.ForkConversationRequest{
		SrcConversationId:  srcCID,
		DestConversationId: destCID,
	})
	if err != nil {
		t.Fatal(err)
	}

	if resp.ConversationId != destCID {
		t.Fatalf("expected destCID %s, got %s", destCID, resp.ConversationId)
	}

	events, err := log.Events(ctx, destCID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
}

// TestServer_Fork_RequiresDestID verifies that ForkConversation rejects
// a request without DestConversationId. The substrate router relies on
// the dest ID to bring up the conversation's actor before the handler
// runs, so we can't accept empty IDs at this layer.
func TestServer_Fork_RequiresDestID(t *testing.T) {
	ctx := context.Background()
	srcCID := "src-conv"

	log := &executortest.MemoryEventLog{}
	log.AllEvents = []*proto.ConversationEvent{
		{
			ConversationId: srcCID,
			Seq:            1,
			Messages: []*proto.Message{
				{Role: "user", Content: &proto.Content{Type: &proto.Content_Text{Text: &proto.TextContent{Text: "msg 1"}}}},
			},
			State: proto.State_STATE_COMPLETED,
		},
	}

	c, err := controller2.New(ctx, controller2.Config{
		EventLogBuilder: func() (executor.EventLog, error) {
			return log, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s := New(c)

	if _, err := s.ForkConversation(ctx, &proto.ForkConversationRequest{
		SrcConversationId: srcCID,
		// DestConversationId intentionally left empty.
	}); err == nil {
		t.Fatal("expected InvalidArgument when DestConversationId is empty, got nil")
	}
}
