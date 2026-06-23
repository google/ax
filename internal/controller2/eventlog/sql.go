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

package eventlog

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/ax/proto"
)

// sqlEventLog is a database backed EventLog shared by the SQLite and
// PostgreSQL implementations.
type sqlEventLog struct {
	db *sql.DB
}

// Append serializes the event to JSON and inserts it into the database.
func (l *sqlEventLog) Append(ctx context.Context, event *proto.ConversationEvent) (int32, error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("eventlog: begin tx: %w", err)
	}
	defer tx.Rollback()

	seq := event.Seq
	if seq == 0 {
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq), 0) + 1 FROM conversation_log WHERE conversation_id = $1", event.ConversationId).Scan(&seq); err != nil {
			return 0, fmt.Errorf("eventlog: compute seq: %w", err)
		}
		event.Seq = seq
	}

	payload, err := marshalOpts.Marshal(event)
	if err != nil {
		return 0, fmt.Errorf("eventlog: marshal event: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		"INSERT INTO conversation_log (conversation_id, seq, payload) VALUES ($1, $2, $3)",
		event.ConversationId, event.Seq, string(payload)); err != nil {
		return 0, fmt.Errorf("eventlog: insert conversation: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("eventlog: commit: %w", err)
	}

	return seq, nil
}

// AppendExec inserts an execution event into the database.
// TODO(anj): Remove execution_log table and AppendExec when legacy controller is removed.
func (l *sqlEventLog) AppendExec(ctx context.Context, event *proto.ExecutionEvent) error {
	payload, err := marshalOpts.Marshal(event)
	if err != nil {
		return fmt.Errorf("eventlog: marshal exec: %w", err)
	}

	var timestamp time.Time
	if event.Timestamp != nil {
		timestamp = event.Timestamp.AsTime()
	} else {
		timestamp = time.Now()
	}

	if _, err := l.db.ExecContext(ctx,
		"INSERT INTO execution_log (exec_id, payload, timestamp) VALUES ($1, $2, $3)",
		event.ExecId, string(payload), timestamp); err != nil {
		return fmt.Errorf("eventlog: insert exec: %w", err)
	}

	return nil
}

// Events retrieves all events from the database for a conversation, ordered by seq.
func (l *sqlEventLog) Events(ctx context.Context, conversationID string) ([]*proto.ConversationEvent, error) {
	rows, err := l.db.QueryContext(ctx, "SELECT payload FROM conversation_log WHERE conversation_id = $1 ORDER BY seq", conversationID)
	if err != nil {
		return nil, fmt.Errorf("eventlog: query conversation: %w", err)
	}
	defer rows.Close()

	var events []*proto.ConversationEvent
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("eventlog: scan conversation: %w", err)
		}

		ev := &proto.ConversationEvent{}
		if err := unmarshalOpts.Unmarshal([]byte(payload), ev); err != nil {
			return nil, fmt.Errorf("eventlog: unmarshal event: %w", err)
		}
		events = append(events, ev)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eventlog: iterate conversation: %w", err)
	}

	return events, nil
}

// ExecEvents retrieves all events from the database for a specific execution ID.
func (l *sqlEventLog) ExecEvents(ctx context.Context, execID string) ([]*proto.ExecutionEvent, error) {
	rows, err := l.db.QueryContext(ctx, "SELECT payload FROM execution_log WHERE exec_id = $1 ORDER BY timestamp", execID)
	if err != nil {
		return nil, fmt.Errorf("eventlog: query exec: %w", err)
	}
	defer rows.Close()

	var events []*proto.ExecutionEvent
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("eventlog: scan exec: %w", err)
		}

		ev := &proto.ExecutionEvent{}
		if err := unmarshalOpts.Unmarshal([]byte(payload), ev); err != nil {
			continue
		}
		events = append(events, ev)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("eventlog: iterate exec: %w", err)
	}

	return events, nil
}

// DeleteAll deletes all events for a specific conversation ID and its child executions.
func (l *sqlEventLog) DeleteAll(ctx context.Context, conversationID string) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("eventlog: begin tx: %w", err)
	}
	defer tx.Rollback()

	// TODO(jbd): Update the schema to include conversation_id at every execution event.

	// Get all exec_ids for this conversation.
	rows, err := tx.QueryContext(ctx, "SELECT payload FROM conversation_log WHERE conversation_id = $1", conversationID)
	if err != nil {
		return fmt.Errorf("eventlog: query conversation: %w", err)
	}
	defer rows.Close()

	var execIDs []string
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return fmt.Errorf("eventlog: scan conversation: %w", err)
		}

		ev := &proto.ConversationEvent{}
		if err := unmarshalOpts.Unmarshal([]byte(payload), ev); err != nil {
			return fmt.Errorf("eventlog: unmarshal event: %w", err)
		}
		if ev.ExecId != "" {
			execIDs = append(execIDs, ev.ExecId)
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("eventlog: iterate conversation: %w", err)
	}
	rows.Close()

	// Delete from execution_log.
	for _, execID := range execIDs {
		if _, err := tx.ExecContext(ctx, "DELETE FROM execution_log WHERE exec_id = $1", execID); err != nil {
			return fmt.Errorf("eventlog: delete exec %s: %w", execID, err)
		}
	}

	// Delete from conversation_log.
	if _, err := tx.ExecContext(ctx, "DELETE FROM conversation_log WHERE conversation_id = $1", conversationID); err != nil {
		return fmt.Errorf("eventlog: delete conversation: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("eventlog: commit tx: %w", err)
	}

	return nil
}

// Close releases the database connection.
func (l *sqlEventLog) Close() error {
	return l.db.Close()
}
