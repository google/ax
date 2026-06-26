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

// Package harness defines a common execution boundary and lifecycle hooks
// for interacting with agents, planners, or external gRPC services.
package harness

import (
	"context"

	"github.com/google/ax/proto"
)

// Handler defines the streaming event hook callbacks for an execution turn.
type Handler interface {
	// OnMessage is invoked when the agent generates output content during its turn.
	OnMessage(ctx context.Context, execID string, msg *proto.Message) error

	// OnComplete is invoked when the agent finishes its current execution turn.
	OnComplete(ctx context.Context, execID string) error
}

// Harness represents a service capable of starting execution sessions.
type Harness interface {
	// Start initializes a new Execution session for a conversation. harnessConfig
	// carries optional per-request harness configuration; it is opaque to the
	// controller and interpreted by the harness implementation.
	Start(ctx context.Context, conversationID string, harnessConfig []byte) (Execution, error)
}

// Execution represents an active interactive session with an agent or planner.
type Execution interface {
	// Run executes the session and streams events to the provided Handler.
	// It blocks until the current turn completes or fails.
	Run(ctx context.Context, handler Handler) error

	// Queue enqueues new input messages to be processed in the next turn.
	Queue(ctx context.Context, msg ...*proto.Message) error

	// ID returns the unique execution session ID.
	ID() string

	// Close cleanly releases all resources associated with the execution session.
	Close(ctx context.Context) error
}
