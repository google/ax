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

package harnesstest

import (
	"context"
	"sync"

	"github.com/google/ax/internal/harness"
	"github.com/google/ax/proto"
	"github.com/google/uuid"
)

// TODO(jbd): Remove once the Harness refractor is done.

// Harness implements the harness.Harness interface for testing purposes.
type Harness struct {
	mu     sync.Mutex
	Active map[string]*Execution
}

// New creates a new Harness instance.
func New() *Harness {
	return &Harness{
		Active: make(map[string]*Execution),
	}
}

// Start implements harness.Harness.
func (h *Harness) Start(ctx context.Context, conversationID string) (harness.Execution, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	execID := uuid.NewString()
	exec := &Execution{
		harness:        h,
		conversationID: conversationID,
		id:             execID,
	}
	h.Active[execID] = exec
	return exec, nil
}

// Execution implements the harness.Execution interface.
type Execution struct {
	harness        *Harness
	conversationID string
	id             string

	mu     sync.Mutex
	queued []*proto.Message
	closed bool
}

// ID implements harness.Execution.
func (e *Execution) ID() string {
	return e.id
}

// Queue implements harness.Execution.
func (e *Execution) Queue(ctx context.Context, msg ...*proto.Message) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.queued = append(e.queued, msg...)
	return nil
}

// Run implements harness.Execution.
// It generates a "Hello world" message and completes the turn.
func (e *Execution) Run(ctx context.Context, handler harness.Handler) error {
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

	if err := handler.OnMessage(ctx, e.id, msg); err != nil {
		return err
	}

	return handler.OnComplete(ctx, e.id)
}

// Close implements harness.Execution.
func (e *Execution) Close(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true

	e.harness.mu.Lock()
	delete(e.harness.Active, e.id)
	e.harness.mu.Unlock()

	return nil
}
