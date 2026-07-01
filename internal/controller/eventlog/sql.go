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

	"github.com/google/ax/proto"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// sqlEventLog is a database backed EventLog shared by the SQLite and
// PostgreSQL implementations.
type sqlEventLog struct {
	db *sql.DB
}

// Append serializes the event to JSON and inserts it into the database.
func (l *sqlEventLog) Append(ctx context.Context, event *proto.ConversationEvent) (seq int32, err error) {
	ctx, endSpan := l.startSpan(ctx, "Append", event.ConversationId)
	defer func() { endSpan(err) }()

	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("eventlog: begin tx: %w", err)
	}
	defer tx.Rollback()

	seq = event.Seq
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

// Events retrieves all events from the database for a conversation, ordered by seq.
func (l *sqlEventLog) Events(ctx context.Context, conversationID string) (events []*proto.ConversationEvent, err error) {
	ctx, endSpan := l.startSpan(ctx, "Events", conversationID)
	defer func() { endSpan(err) }()

	rows, err := l.db.QueryContext(ctx, "SELECT payload FROM conversation_log WHERE conversation_id = $1 ORDER BY seq", conversationID)
	if err != nil {
		return nil, fmt.Errorf("eventlog: query conversation: %w", err)
	}
	defer rows.Close()

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

// DeleteAll deletes all events for a specific conversation ID.
func (l *sqlEventLog) DeleteAll(ctx context.Context, conversationID string) (err error) {
	ctx, endSpan := l.startSpan(ctx, "DeleteAll", conversationID)
	defer func() { endSpan(err) }()

	if _, err := l.db.ExecContext(ctx, "DELETE FROM conversation_log WHERE conversation_id = $1", conversationID); err != nil {
		return fmt.Errorf("eventlog: delete conversation: %w", err)
	}
	return nil
}

// Close releases the database connection.
func (l *sqlEventLog) Close() error {
	return l.db.Close()
}

func (l *sqlEventLog) startSpan(ctx context.Context, name string, conversationID string) (context.Context, func(err error)) {
	const tracerName = "eventlog.sql"
	tracer := otel.Tracer(tracerName)

	ctx, span := tracer.Start(ctx, tracerName+"/"+name, trace.WithAttributes(
		attribute.String("conversation_id", conversationID),
	))
	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
}
