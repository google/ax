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

package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

// testURL returns the database URL from $AX_TEST_POSTGRES_URL, skipping the
// test when it is unset. Tests drop and recreate all AX tables in it.
func testURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("AX_TEST_POSTGRES_URL")
	if u == "" {
		t.Skip("AX_TEST_POSTGRES_URL not set; skipping Postgres tests")
	}
	return u
}

// openTest opens a Store on an empty schema.
func openTest(t *testing.T, opts Options) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, testURL(t), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `TRUNCATE tasks, workspaces, models, locks`); err != nil {
		t.Fatalf("truncating tables: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestCreateSchemaConcurrent(t *testing.T) {
	s := openTest(t, Options{})
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `DROP TABLE tasks, workspaces, models, locks`); err != nil {
		t.Fatal(err)
	}
	// Simulate several replicas starting at once.
	errs := make(chan error, 4)
	for range 4 {
		go func() { errs <- createSchema(ctx, s.pool) }()
	}
	for range 4 {
		if err := <-errs; err != nil {
			t.Fatalf("createSchema: %v", err)
		}
	}
	if err := s.SaveTask(ctx, &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: "after-create"}}); err != nil {
		t.Fatalf("SaveTask after createSchema: %v", err)
	}
}

// TestListNewestFirst checks that lists are ordered by most recent save, as
// they were with Redis: re-saving a resource moves it to the front, and a
// status update does not.
func TestListNewestFirst(t *testing.T) {
	s := openTest(t, Options{})
	ctx := context.Background()
	save := func(atespace, name string) {
		t.Helper()
		task := &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Atespace: atespace, Name: name}}
		if err := s.SaveTask(ctx, task); err != nil {
			t.Fatalf("SaveTask: %v", err)
		}
	}
	save("a", "a1")
	save("a", "a2")
	save("a", "a3")
	save("b", "b1")
	save("a", "a1")
	if err := s.UpdateTaskStatus(ctx, "a", "a2", &v1alpha1.TaskStatus{Phase: v1alpha1.PhaseFailed}); err != nil {
		t.Fatalf("UpdateTaskStatus: %v", err)
	}

	check := func(atespace string, limit, offset int64, want ...string) {
		t.Helper()
		tasks, err := s.ListTasks(ctx, atespace, limit, offset)
		if err != nil {
			t.Fatalf("ListTasks(%q, %d, %d): %v", atespace, limit, offset, err)
		}
		var got []string
		for _, task := range tasks {
			got = append(got, task.GetMetadata().GetAtespace()+"/"+task.GetMetadata().GetName())
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("ListTasks(%q, %d, %d) = %v, want %v", atespace, limit, offset, got, want)
		}
	}
	check("a", 10, 0, "a/a1", "a/a3", "a/a2")
	check("*", 10, 0, "a/a1", "b/b1", "a/a3", "a/a2")
	check("*", 2, 2, "a/a3", "a/a2")
	check("a", 1, 1, "a/a3")

	for _, w := range []string{"w1", "w2"} {
		if err := s.SaveWorkspace(ctx, &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: w}}); err != nil {
			t.Fatalf("SaveWorkspace: %v", err)
		}
	}
	if list, err := s.ListWorkspaces(ctx, "*"); err != nil || len(list) != 2 || list[0].GetMetadata().GetName() != "w2" {
		t.Errorf("ListWorkspaces = %v, %v; want w2 first", list, err)
	}
	for _, m := range []string{"m1", "m2"} {
		if err := s.SaveModel(ctx, &v1alpha1.Model{Metadata: &v1alpha1.ObjectMeta{Name: m}}); err != nil {
			t.Fatalf("SaveModel: %v", err)
		}
	}
	if list, err := s.ListModels(ctx, "*"); err != nil || len(list) != 2 || list[0].GetMetadata().GetName() != "m2" {
		t.Errorf("ListModels = %v, %v; want m2 first", list, err)
	}
}

// TestLockRenewsLeaseWhileHeld verifies that a held lock outlives its TTL
// (because it is renewed) and that waiters acquire it only after release.
func TestLockRenewsLeaseWhileHeld(t *testing.T) {
	ttl := 300 * time.Millisecond
	s := openTest(t, Options{LockTTL: ttl, LockRetryInterval: 20 * time.Millisecond})
	ctx := context.Background()

	unlock, err := s.Lock(ctx, "task", "default", "renew")
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 4*ttl)
	defer cancel()
	if _, err := s.Lock(waitCtx, "task", "default", "renew"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected contender deadline while lease was held, got %v", err)
	}
	unlock()

	acquireCtx, cancel2 := context.WithTimeout(ctx, ttl)
	defer cancel2()
	unlock2, err := s.Lock(acquireCtx, "task", "default", "renew")
	if err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	unlock2()
}

// TestLockTakesOverExpiredLease verifies that a lock orphaned by a crashed
// holder becomes available once its lease expires.
func TestLockTakesOverExpiredLease(t *testing.T) {
	ttl := 200 * time.Millisecond
	s := openTest(t, Options{LockTTL: ttl, LockRetryInterval: 20 * time.Millisecond})
	ctx := context.Background()

	// Start the clock before the INSERT. The lease expires ttl after the
	// database evaluates clock_timestamp(), which happens somewhere during the
	// INSERT's round trip, so timing from after it returns would undercount
	// by however long the round trip and commit took.
	start := time.Now()
	if _, err := s.pool.Exec(ctx, `INSERT INTO locks (key, token, expires_at) VALUES ($1, 'crashed', clock_timestamp() + $2 * interval '1 millisecond')`,
		"lock:task:default:orphan", ttl.Milliseconds()); err != nil {
		t.Fatal(err)
	}
	acquireCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	unlock, err := s.Lock(acquireCtx, "task", "default", "orphan")
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	defer unlock()
	if elapsed := time.Since(start); elapsed < ttl {
		t.Fatalf("acquired orphaned lock after %v, before its %v lease expired", elapsed, ttl)
	}
}

// TestRestartKeepsTasksAndReleasesLocks simulates a server crash. A task is
// saved and a lock is taken, then the store is closed without unlocking and
// reopened on the same database. The task must still be there, and the lock
// must be takeable once its lease runs out.
func TestRestartKeepsTasksAndReleasesLocks(t *testing.T) {
	ttl := 300 * time.Millisecond
	opts := Options{LockTTL: ttl, LockRetryInterval: 20 * time.Millisecond}
	s1 := openTest(t, opts)
	ctx := context.Background()
	task := &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Atespace: "default", Name: "survivor"}}
	if err := s1.SaveTask(ctx, task); err != nil {
		t.Fatalf("SaveTask: %v", err)
	}
	if _, err := s1.Lock(ctx, "task", "default", "survivor"); err != nil {
		t.Fatalf("Lock before restart: %v", err)
	}
	// Crash: close without calling unlock, so the lease is never released.
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen without truncating, so the data from before the restart is kept.
	s2, err := Open(ctx, testURL(t), opts)
	if err != nil {
		t.Fatalf("Open after restart: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	got, err := s2.GetTask(ctx, "default", "survivor")
	if err != nil {
		t.Fatalf("GetTask after restart: %v", err)
	}
	if got.GetMetadata().GetName() != "survivor" {
		t.Fatalf("GetTask name = %q, want survivor", got.GetMetadata().GetName())
	}

	acquireCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	unlock, err := s2.Lock(acquireCtx, "task", "default", "survivor")
	if err != nil {
		t.Fatalf("Lock after restart: %v", err)
	}
	unlock()
}
