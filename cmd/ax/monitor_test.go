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

package main

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestFetchConversations(t *testing.T) {
	// Create an in-memory SQLite database for testing
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open in-memory db: %v", err)
	}
	defer db.Close()

	// Initialize the schema
	schema := `
	CREATE TABLE conversation_log (
		conversation_id TEXT,
		seq INTEGER,
		timestamp DATETIME,
		payload TEXT
	);

	CREATE TABLE execution_log (
		exec_id TEXT,
		timestamp DATETIME,
		payload TEXT
	);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("failed to create schema: %v", err)
	}

	// Insert test data
	// Conversation 1: Only conversation_log (V2 execution without execution_log)
	_, err = db.Exec(`
		INSERT INTO conversation_log (conversation_id, seq, timestamp, payload) 
		VALUES ('conv-1', 1, ?, '{"exec_id": "exec-1", "state": "STATE_PENDING"}')
	`, time.Now().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("failed to insert conv-1: %v", err)
	}

	// Conversation 2: Both conversation_log and execution_log (V1 execution)
	now := time.Now()
	start := now.Add(-5 * time.Second)
	end := now

	_, err = db.Exec(`
		INSERT INTO conversation_log (conversation_id, seq, timestamp, payload) 
		VALUES ('conv-2', 5, ?, '{"exec_id": "exec-2", "state": "STATE_COMPLETED"}')
	`, end.Format(time.RFC3339))
	if err != nil {
		t.Fatalf("failed to insert conv-2: %v", err)
	}

	_, err = db.Exec(`
		INSERT INTO execution_log (exec_id, timestamp, payload) 
		VALUES 
		('exec-2', ?, '{"agent_id": "my-agent"}'),
		('exec-2', ?, '{"agent_id": "my-agent"}')
	`, start.Format(time.RFC3339), end.Format(time.RFC3339))
	if err != nil {
		t.Fatalf("failed to insert exec-2: %v", err)
	}

	// Fetch the conversations
	ctx := context.Background()
	convs, err := fetchConversations(ctx, db)
	if err != nil {
		t.Fatalf("fetchConversations failed: %v", err)
	}

	if len(convs) != 2 {
		t.Fatalf("expected 2 conversations, got %d", len(convs))
	}

	// Create a map for easy lookup
	convMap := make(map[string]ConversationResponse)
	for _, c := range convs {
		convMap[c.ID] = c
	}

	// Check conv-1
	c1, ok := convMap["conv-1"]
	if !ok {
		t.Fatalf("conv-1 not found")
	}
	if c1.Status != "RUNNING" { // STATE_PENDING -> RUNNING
		t.Errorf("conv-1 expected status RUNNING, got %q", c1.Status)
	}
	if c1.Agent != "unknown" {
		t.Errorf("conv-1 expected agent unknown, got %q", c1.Agent)
	}
	if c1.Duration != "N/A" {
		t.Errorf("conv-1 expected duration N/A, got %q", c1.Duration)
	}
	if c1.LastSeq != 1 {
		t.Errorf("conv-1 expected last_seq 1, got %d", c1.LastSeq)
	}

	// Check conv-2
	c2, ok := convMap["conv-2"]
	if !ok {
		t.Fatalf("conv-2 not found")
	}
	if c2.Status != "COMPLETED" { // STATE_COMPLETED -> COMPLETED
		t.Errorf("conv-2 expected status COMPLETED, got %q", c2.Status)
	}
	if c2.Agent != "my-agent" {
		t.Errorf("conv-2 expected agent my-agent, got %q", c2.Agent)
	}
	// Duration should be roughly 5.0s
	if c2.Duration != "5.0s" {
		t.Errorf("conv-2 expected duration 5.0s, got %q", c2.Duration)
	}
	if c2.LastSeq != 5 {
		t.Errorf("conv-2 expected last_seq 5, got %d", c2.LastSeq)
	}
}
