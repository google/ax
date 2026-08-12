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
	AllEvents     []*proto.StepEvent
}

func (m *MemoryEventLog) Append(_ context.Context, event *proto.StepEvent) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	maxStep := int64(0)
	for _, ev := range m.AllEvents {
		if ev.ConversationId == event.ConversationId {
			maxStep++
		}
	}
	step := maxStep + 1
	m.AllEvents = append(m.AllEvents, event)
	return step, nil
}

func (m *MemoryEventLog) Events(_ context.Context, conversationID string) ([]*proto.StepEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]*proto.StepEvent, 0)
	for _, ev := range m.AllEvents {
		if ev.ConversationId == conversationID {
			out = append(out, ev)
		}
	}
	return out, nil
}

// Drop removes every event for which drop returns true.
// It is provided for testing and crash-simulation purposes.
func (m *MemoryEventLog) Drop(drop func(*proto.StepEvent) bool) {
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

	var keptEvents []*proto.StepEvent
	for _, ev := range m.AllEvents {
		if ev.ConversationId != conversationID {
			keptEvents = append(keptEvents, ev)
		}
	}
	m.AllEvents = keptEvents

	return nil
}

func (m *MemoryEventLog) Close() error {
	return nil
}
