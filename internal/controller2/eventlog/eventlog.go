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

// Package eventlog contains an AX event log.
package eventlog

import (
	"context"

	"github.com/google/ax/proto"
	"google.golang.org/protobuf/encoding/protojson"
)

// EventLogBuilder builds new event logs.
type EventLogBuilder func() (EventLog, error)

// EventLog is the persistent, append-only record of all actions taken in an
// exec. Every entry is an atomic step: replaying the log in order brings
// the executor back to a consistent state from which execution can resume.
type EventLog interface {
	// Append adds a conversation event to the end of the log.
	Append(ctx context.Context, event *proto.ConversationEvent) (int32, error)

	// AppendExec adds an execution event to the end of the log.
	AppendExec(ctx context.Context, event *proto.ExecutionEvent) error

	// Events returns all events for the conversation.
	Events(ctx context.Context, conversationID string) ([]*proto.ConversationEvent, error)

	// ExecEvents returns all events for a specific execution ID.
	ExecEvents(ctx context.Context, execID string) ([]*proto.ExecutionEvent, error)

	// DeleteAll deletes all events for a specific conversation ID.
	DeleteAll(ctx context.Context, conversationID string) error

	// Close releases the underlying resources and closes the log.
	Close() error
}

var marshalOpts = protojson.MarshalOptions{UseProtoNames: true}
var unmarshalOpts = protojson.UnmarshalOptions{DiscardUnknown: true}
