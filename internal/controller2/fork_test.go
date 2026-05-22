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

	"github.com/google/ax/internal/controller/executor"
	"github.com/google/ax/internal/controller/executor/executortest"
	"github.com/google/ax/proto"
)

func TestController_Fork(t *testing.T) {
	ctx := context.Background()
	srcCID := "src-conv"
	destCID := "dest-conv"

	log := &executortest.MemoryEventLog{}
	// Pre-populate history
	log.AllEvents = []*proto.ConversationEvent{
		{
			ConversationId: srcCID,
			Seq:            1,
			Messages: []*proto.Message{
				{Role: "user", Content: &proto.Content{Type: &proto.Content_Text{Text: &proto.TextContent{Text: "msg 1"}}}},
			},
			State: proto.State_STATE_COMPLETED,
		},
		{
			ConversationId: srcCID,
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
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Case 1: Fork without seq (should copy all)
	forkedID, err := c.Fork(ctx, srcCID, 0, destCID)
	if err != nil {
		t.Fatal(err)
	}
	if forkedID != destCID {
		t.Fatalf("expected destCID %s, got %s", destCID, forkedID)
	}

	events, err := log.Events(ctx, destCID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Seq != 1 || events[1].Seq != 2 {
		t.Errorf("sequences mismatch")
	}

	// Case 2: Fork with seq 1
	destCID2 := "dest-conv-2"
	forkedID2, err := c.Fork(ctx, srcCID, 1, destCID2)
	if err != nil {
		t.Fatal(err)
	}
	if forkedID2 != destCID2 {
		t.Fatalf("expected destCID2 %s, got %s", destCID2, forkedID2)
	}

	events2, err := log.Events(ctx, destCID2)
	if err != nil {
		t.Fatal(err)
	}
	if len(events2) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events2))
	}
	if events2[0].Seq != 1 {
		t.Errorf("expected seq 1, got %d", events2[0].Seq)
	}
}

func TestController_Fork_SrcSeqNotFound(t *testing.T) {
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
		{
			ConversationId: srcCID,
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
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Non-existent src_seq must error and write nothing.
	if _, err := c.Fork(ctx, srcCID, 99, "dest-conv"); err == nil {
		t.Fatal("expected error when src_seq does not exist, got nil")
	}
	events, err := log.Events(ctx, "dest-conv")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events on failed fork, got %d", len(events))
	}

	// src_seq exactly at the max is valid and must succeed.
	if _, err := c.Fork(ctx, srcCID, 2, "dest-conv-boundary"); err != nil {
		t.Fatalf("unexpected error forking at boundary src_seq: %v", err)
	}
}
