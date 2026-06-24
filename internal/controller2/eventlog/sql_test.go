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
	"os"
	"path/filepath"
	"testing"

	"github.com/google/ax/proto"
)

// testEventLog runs the EventLog contract against a backend. newLog returns a
// fresh, empty event log for each subtest, using the subtest's *testing.T for
// temp dirs and cleanup.
//
// Subtests derive their IDs from t.Name() (which includes the backend and
// subtest name) and clear them with DeleteAll before and after running, so they
// are safe against the shared Postgres database and harmless against SQLite's
// per-test file.
func testEventLog(t *testing.T, newLog func(t *testing.T) EventLog) {
	t.Run("AppendAndEvents", func(t *testing.T) {
		ctx := context.Background()
		log := newLog(t)

		conv := t.Name() + "-conv-1"
		task1 := t.Name() + "-task-1"
		task2 := t.Name() + "-task-2"
		_ = log.DeleteAll(ctx, conv)
		t.Cleanup(func() { _ = log.DeleteAll(ctx, conv) })

		// 1. Conversation log.
		cev1 := &proto.ConversationEvent{ConversationId: conv, Seq: 1, ExecId: task1}
		cev2 := &proto.ConversationEvent{ConversationId: conv, Seq: 2, ExecId: task2}
		if _, err := log.Append(ctx, cev1); err != nil {
			t.Fatalf("failed to append cev1: %v", err)
		}
		if _, err := log.Append(ctx, cev2); err != nil {
			t.Fatalf("failed to append cev2: %v", err)
		}

		cEvents, err := log.Events(ctx, conv)
		if err != nil {
			t.Fatalf("failed to read conversation events: %v", err)
		}
		if len(cEvents) != 2 {
			t.Fatalf("expected 2 conversation events, got %d", len(cEvents))
		}
		if cEvents[0].Seq != 1 || cEvents[1].Seq != 2 {
			t.Errorf("conversation events out of order: %d, %d", cEvents[0].Seq, cEvents[1].Seq)
		}
		if cEvents[0].ExecId != task1 || cEvents[1].ExecId != task2 {
			t.Errorf("conversation events mismatch: %q, %q", cEvents[0].ExecId, cEvents[1].ExecId)
		}


	})

	t.Run("Empty", func(t *testing.T) {
		ctx := context.Background()
		log := newLog(t)

		events, err := log.Events(ctx, t.Name()+"-none")
		if err != nil {
			t.Fatalf("failed to read events: %v", err)
		}
		if len(events) != 0 {
			t.Fatalf("expected 0 events, got %d", len(events))
		}
	})

	t.Run("DeleteAll", func(t *testing.T) {
		ctx := context.Background()
		log := newLog(t)

		conv1 := t.Name() + "-conv-1"
		conv2 := t.Name() + "-conv-2"
		task1 := t.Name() + "-task-1"
		task3 := t.Name() + "-task-3"
		_ = log.DeleteAll(ctx, conv1)
		_ = log.DeleteAll(ctx, conv2)
		t.Cleanup(func() {
			_ = log.DeleteAll(ctx, conv1)
			_ = log.DeleteAll(ctx, conv2)
		})

		if _, err := log.Append(ctx, &proto.ConversationEvent{ConversationId: conv1, Seq: 1, ExecId: task1}); err != nil {
			t.Fatalf("append: %v", err)
		}
		if _, err := log.Append(ctx, &proto.ConversationEvent{ConversationId: conv2, Seq: 1, ExecId: task3}); err != nil {
			t.Fatalf("append: %v", err)
		}


		if err := log.DeleteAll(ctx, conv1); err != nil {
			t.Fatalf("failed to delete events: %v", err)
		}

		if ev, _ := log.Events(ctx, conv1); len(ev) != 0 {
			t.Errorf("expected 0 events for conv1, got %d", len(ev))
		}
		if ev, _ := log.Events(ctx, conv2); len(ev) != 1 {
			t.Errorf("expected 1 event for conv2, got %d", len(ev))
		}

	})

	// AutoSeq exercises the seq==0 auto-assignment path: appends with Seq unset
	// receive sequential numbers starting at 1.
	t.Run("AutoSeq", func(t *testing.T) {
		ctx := context.Background()
		log := newLog(t)

		conv := t.Name() + "-conv"
		_ = log.DeleteAll(ctx, conv)
		t.Cleanup(func() { _ = log.DeleteAll(ctx, conv) })

		const n = 3
		for i := int32(1); i <= n; i++ {
			seq, err := log.Append(ctx, &proto.ConversationEvent{ConversationId: conv, ExecId: "t"})
			if err != nil {
				t.Fatalf("auto-seq append failed: %v", err)
			}
			if seq != i {
				t.Errorf("expected seq %d, got %d", i, seq)
			}
		}

		events, err := log.Events(ctx, conv)
		if err != nil {
			t.Fatalf("failed to read events: %v", err)
		}
		if len(events) != n {
			t.Fatalf("expected %d events, got %d", n, len(events))
		}
		for i, e := range events {
			if e.Seq != int32(i+1) {
				t.Errorf("event %d: expected seq %d, got %d", i, i+1, e.Seq)
			}
		}
	})
}

// TestSQLiteEventLog runs the EventLog contract against the SQLite backend.
func TestSQLiteEventLog(t *testing.T) {
	testEventLog(t, func(t *testing.T) EventLog {
		t.Helper()
		log, err := OpenSQLiteEventLog(filepath.Join(t.TempDir(), "test.db"))
		if err != nil {
			t.Fatalf("failed to open sqlite event log: %v", err)
		}
		t.Cleanup(func() { log.Close() })
		return log
	})
}

// TestPostgresEventLog runs the EventLog contract against the Postgres backend
// described by PG_TEST_DSN, skipping when that variable is not set.
func TestPostgresEventLog(t *testing.T) {
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping Postgres event log tests")
	}
	testEventLog(t, func(t *testing.T) EventLog {
		t.Helper()
		log, err := OpenPostgresEventLog(dsn)
		if err != nil {
			t.Fatalf("failed to open postgres event log: %v", err)
		}
		t.Cleanup(func() { log.Close() })
		return log
	})
}

// TestSQLiteEventLog_CreatesParentDirectory is SQLite-specific: OpenSQLiteEventLog
// must create the database file's parent directory if it does not exist.
func TestSQLiteEventLog_CreatesParentDirectory(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "newdir", "test.db")

	log, err := OpenSQLiteEventLog(dbPath)
	if err != nil {
		t.Fatalf("failed to open sqlite event log and create directory: %v", err)
	}
	defer log.Close()

	if _, err := os.Stat(filepath.Dir(dbPath)); os.IsNotExist(err) {
		t.Fatalf("expected parent directory to be created, but it does not exist")
	}
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Fatalf("expected database file to be created, but it does not exist")
	}
}
