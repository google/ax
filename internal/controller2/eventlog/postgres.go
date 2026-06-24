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

	_ "github.com/jackc/pgx/v5/stdlib"
)

// OpenPostgresEventLog connects to the PostgreSQL database described by dsn and
// initializes the event log schema. Caller is responsible to ensure it is safe
// for concurrent use.
func OpenPostgresEventLog(dsn string) (EventLog, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres_eventlog: open: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("postgres_eventlog: ping: %w", err)
	}

	// Create tables if they don't exist.
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS conversation_log (
			conversation_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (conversation_id, seq)
		)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("postgres_eventlog: create conversation_log table: %w", err)
	}

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS execution_log (
			exec_id TEXT NOT NULL,
			payload TEXT NOT NULL,
			timestamp TIMESTAMPTZ NOT NULL
		)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("postgres_eventlog: create execution_log table: %w", err)
	}

	// Create indexes if they don't exist.
	if _, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_execution_log_exec_id ON execution_log(exec_id)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("postgres_eventlog: create index exec_id: %w", err)
	}

	return &sqlEventLog{db: db}, nil
}
