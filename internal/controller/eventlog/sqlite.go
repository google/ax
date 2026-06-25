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
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const sqliteBusyTimeout = 10 * time.Second

// OpenSQLiteEventLog opens (or creates) a SQLite database at path and initializes the event log schema.
func OpenSQLiteEventLog(path string) (EventLog, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("sqlite_eventlog: mkdir %s: %w", dir, err)
	}

	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(%d)&_txlock=immediate", path, sqliteBusyTimeout.Milliseconds())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite_eventlog: open %s: %w", dsn, err)
	}

	// Create tables if they don't exist.
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


	return &sqlEventLog{db: db}, nil
}
