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

package executor

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/ax/proto"
	_ "modernc.org/sqlite"
)

// SQLiteEventLog is a durable EventLog that persists events in a SQLite database.
// It is safe for concurrent use.
type SQLiteEventLog struct {
	db *sql.DB
}

const sqliteBusyTimeout = 10 * time.Second

// OpenSQLiteEventLog opens (or creates) a SQLite database at path and initializes the event log schema.
func OpenSQLiteEventLog(path string) (*SQLiteEventLog, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("sqlite_eventlog: mkdir %s: %w", dir, err)
	}

	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(%d)", path, sqliteBusyTimeout.Milliseconds())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite_eventlog: open %s: %w", dsn, err)
	}

	// Create tables if they don't exist
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS conversation_log (
			conversation_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (conversation_id, seq)
		)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite_eventlog: create conversation_log table: %w", err)
	}

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS execution_log (
			exec_id TEXT NOT NULL,
			payload TEXT NOT NULL,
			timestamp DATETIME NOT NULL
		)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite_eventlog: create execution_log table: %w", err)
	}

	// Create indexes if they don't exist
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_execution_log_exec_id ON execution_log(exec_id)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite_eventlog: create index exec_id: %w", err)
	}

	return &SQLiteEventLog{db: db}, nil
}

// Append serializes the event to JSON and inserts it into the database.
func (l *SQLiteEventLog) Append(ctx context.Context, event *proto.ConversationEvent) (int32, error) {
	seq := event.Seq
	if seq == 0 {
		err := l.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq), 0) + 1 FROM conversation_log WHERE conversation_id = ?", event.ConversationId).Scan(&seq)
		if err != nil {
			return 0, fmt.Errorf("sqlite_eventlog: compute seq: %w", err)
		}
		event.Seq = seq
	}

	payload, err := marshalOpts.Marshal(event)
	if err != nil {
		return 0, fmt.Errorf("sqlite_eventlog: marshal event: %w", err)
	}

	_, err = l.db.ExecContext(ctx,
		"INSERT INTO conversation_log (conversation_id, seq, payload) VALUES (?, ?, ?)",
		event.ConversationId, event.Seq, string(payload))

	if err != nil {
		return 0, fmt.Errorf("sqlite_eventlog: insert conversation: %w", err)
	}

	return seq, nil
}

// AppendExec inserts an execution event into the database.
// TODO(anj): Remove execution_log table and AppendExec when legacy controller is removed.
func (l *SQLiteEventLog) AppendExec(ctx context.Context, event *proto.ExecutionEvent) error {
	payload, err := marshalOpts.Marshal(event)
	if err != nil {
		return fmt.Errorf("sqlite_eventlog: marshal exec: %w", err)
	}

	var timestamp time.Time
	if event.Timestamp != nil {
		timestamp = event.Timestamp.AsTime()
	} else {
		timestamp = time.Now()
	}

	_, err = l.db.ExecContext(ctx,
		"INSERT INTO execution_log (exec_id, payload, timestamp) VALUES (?, ?, ?)",
		event.ExecId, string(payload), timestamp)

	if err != nil {
		return fmt.Errorf("sqlite_eventlog: insert exec: %w", err)
	}

	return nil
}

// Events retrieves all events from the database for a conversation, ordered by seq and execution order.
func (l *SQLiteEventLog) Events(ctx context.Context, conversationID string) ([]*proto.ConversationEvent, error) {
	rows, err := l.db.QueryContext(ctx, "SELECT payload FROM conversation_log WHERE conversation_id = ? ORDER BY seq", conversationID)
	if err != nil {
		return nil, fmt.Errorf("sqlite_eventlog: query conversation: %w", err)
	}
	defer rows.Close()

	var events []*proto.ConversationEvent
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("sqlite_eventlog: scan conversation: %w", err)
		}

		ev := &proto.ConversationEvent{}
		if err := unmarshalOpts.Unmarshal([]byte(payload), ev); err != nil {
			return nil, fmt.Errorf("sqlite_eventlog: unmarshal event: %w", err)
		}
		events = append(events, ev)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite_eventlog: iterate conversation: %w", err)
	}

	return events, nil
}

// ExecEvents retrieves all events from the database for a specific execution ID.
func (l *SQLiteEventLog) ExecEvents(ctx context.Context, execID string) ([]*proto.ExecutionEvent, error) {
	rows, err := l.db.QueryContext(ctx, "SELECT payload FROM execution_log WHERE exec_id = ? ORDER BY timestamp", execID)
	if err != nil {
		return nil, fmt.Errorf("sqlite_eventlog: query exec: %w", err)
	}
	defer rows.Close()

	var events []*proto.ExecutionEvent
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("sqlite_eventlog: scan exec: %w", err)
		}

		ev := &proto.ExecutionEvent{}
		if err := unmarshalOpts.Unmarshal([]byte(payload), ev); err != nil {
			continue
		}
		events = append(events, ev)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite_eventlog: iterate exec: %w", err)
	}

	return events, nil
}

// DeleteEvents deletes all events for a specific conversation ID and its child executions.
func (l *SQLiteEventLog) DeleteEvents(ctx context.Context, conversationID string) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite_eventlog: begin tx: %w", err)
	}
	defer tx.Rollback()

	// TODO(jbd): Update the schema to include conversation_id at every execution event.

	// Get all exec_ids for this conversation
	rows, err := tx.QueryContext(ctx, "SELECT payload FROM conversation_log WHERE conversation_id = ?", conversationID)
	if err != nil {
		return fmt.Errorf("sqlite_eventlog: query conversation: %w", err)
	}
	defer rows.Close()

	var execIDs []string
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return fmt.Errorf("sqlite_eventlog: scan conversation: %w", err)
		}

		ev := &proto.ConversationEvent{}
		if err := unmarshalOpts.Unmarshal([]byte(payload), ev); err != nil {
			return fmt.Errorf("sqlite_eventlog: unmarshal event: %w", err)
		}
		if ev.ExecId != "" {
			execIDs = append(execIDs, ev.ExecId)
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite_eventlog: iterate conversation: %w", err)
	}

	// Delete from execution_log
	for _, execID := range execIDs {
		if _, err := tx.ExecContext(ctx, "DELETE FROM execution_log WHERE exec_id = ?", execID); err != nil {
			return fmt.Errorf("sqlite_eventlog: delete exec %s: %w", execID, err)
		}
	}

	// Delete from conversation_log
	if _, err := tx.ExecContext(ctx, "DELETE FROM conversation_log WHERE conversation_id = ?", conversationID); err != nil {
		return fmt.Errorf("sqlite_eventlog: delete conversation: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite_eventlog: commit tx: %w", err)
	}

	return nil
}

// Close releases the database connection.
func (l *SQLiteEventLog) Close() error {
	return l.db.Close()
}
