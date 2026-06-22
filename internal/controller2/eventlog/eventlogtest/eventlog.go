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

package eventlogtest

import (
	"context"
	"sync"

	"github.com/google/ax/proto"
)

// MemoryEventLog is an in-memory EventLog useful for testing and short-lived
// executions. It does not survive process restarts.
type MemoryEventLog struct {
	mu            sync.Mutex
	AllEvents     []*proto.ConversationEvent
	AllExecEvents []*proto.ExecutionEvent
}

func (m *MemoryEventLog) Append(_ context.Context, event *proto.ConversationEvent) (int32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	seq := event.Seq
	if seq == 0 {
		maxSeq := int32(0)
		for _, ev := range m.AllEvents {
			if ev.ConversationId == event.ConversationId && ev.Seq > maxSeq {
				maxSeq = ev.Seq
			}
		}
		seq = maxSeq + 1
		event.Seq = seq
	}
	m.AllEvents = append(m.AllEvents, event)
	return seq, nil
}

func (m *MemoryEventLog) AppendExec(_ context.Context, event *proto.ExecutionEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.AllExecEvents = append(m.AllExecEvents, event)
	return nil
}

func (m *MemoryEventLog) Events(_ context.Context, conversationID string) ([]*proto.ConversationEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]*proto.ConversationEvent, 0)
	for _, ev := range m.AllEvents {
		if ev.ConversationId == conversationID {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (m *MemoryEventLog) ExecEvents(_ context.Context, execID string) ([]*proto.ExecutionEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]*proto.ExecutionEvent, 0)
	for _, ev := range m.AllExecEvents {
		if ev.ExecId == execID {
			out = append(out, ev)
		}
	}
	return out, nil
}

// Drop removes every event for which drop returns true.
// It is provided for testing and crash-simulation purposes.
func (m *MemoryEventLog) Drop(drop func(*proto.ConversationEvent) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	kept := m.AllEvents[:0]
	for _, ev := range m.AllEvents {
		if !drop(ev) {
			kept = append(kept, ev)
		}
	}
	m.AllEvents = kept
}

func (m *MemoryEventLog) DeleteAll(_ context.Context, conversationID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var execIDs []string
	var keptEvents []*proto.ConversationEvent
	for _, ev := range m.AllEvents {
		if ev.ConversationId == conversationID {
			if ev.ExecId != "" {
				execIDs = append(execIDs, ev.ExecId)
			}
		} else {
			keptEvents = append(keptEvents, ev)
		}
	}
	m.AllEvents = keptEvents

	var keptExecEvents []*proto.ExecutionEvent
	for _, ev := range m.AllExecEvents {
		delete := false
		for _, id := range execIDs {
			if ev.ExecId == id {
				delete = true
				break
			}
		}
		if !delete {
			keptExecEvents = append(keptExecEvents, ev)
		}
	}
	m.AllExecEvents = keptExecEvents

	return nil
}

func (m *MemoryEventLog) Close() error {
	return nil
}
