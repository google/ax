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

package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/ax/internal/store"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/proto"
)

func openTestStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path, Options{WatchInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func task(atespace, name string) *v1alpha1.Task {
	return &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Atespace: atespace, Name: name},
		Spec:     &v1alpha1.TaskSpec{Image: "img", Env: []*v1alpha1.EnvVar{{Name: "A", Value: "B"}}},
	}
}

func TestOpenCreatesPrivateDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "db.sqlite")
	openTestStore(t, path)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("database not created: %v", err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("database permissions = %v, want owner-only", perm)
	}
}

func TestTaskCRUD(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, filepath.Join(t.TempDir(), "db.sqlite"))

	if _, err := s.GetTask(ctx, "default", "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetTask(missing) err = %v, want ErrNotFound", err)
	}

	in := task("", "t1")
	if err := s.SaveTask(ctx, in); err != nil {
		t.Fatalf("SaveTask: %v", err)
	}
	got, err := s.GetTask(ctx, "default", "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.GetStatus().GetPhase() != "Pending" || got.GetMetadata().GetAtespace() != "default" || got.GetKind() != v1alpha1.KindTask {
		t.Errorf("defaults not applied: %v", got)
	}
	if !proto.Equal(got.GetSpec(), in.GetSpec()) {
		t.Errorf("spec round-trip mismatch: got %v want %v", got.GetSpec(), in.GetSpec())
	}

	st := &v1alpha1.TaskStatus{Phase: "Running", WorkerIp: "127.0.0.1:1234"}
	if err := s.UpdateTaskStatus(ctx, "default", "t1", st); err != nil {
		t.Fatalf("UpdateTaskStatus: %v", err)
	}
	got, _ = s.GetTask(ctx, "default", "t1")
	if !proto.Equal(got.GetStatus(), st) || !proto.Equal(got.GetSpec(), in.GetSpec()) {
		t.Errorf("after status update got %v", got)
	}
	if err := s.UpdateTaskStatus(ctx, "default", "nope", st); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateTaskStatus(missing) err = %v, want ErrNotFound", err)
	}

	if err := s.DeleteTask(ctx, "default", "t1"); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	if _, err := s.GetTask(ctx, "default", "t1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetTask after delete err = %v, want ErrNotFound", err)
	}
}

func TestListTasksNewestFirstAndScoped(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, filepath.Join(t.TempDir(), "db.sqlite"))
	for _, tk := range []*v1alpha1.Task{task("a", "one"), task("a", "two"), task("b", "three")} {
		if err := s.SaveTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	// A status update must not reorder listings.
	if err := s.UpdateTaskStatus(ctx, "a", "one", &v1alpha1.TaskStatus{Phase: "Running"}); err != nil {
		t.Fatal(err)
	}

	names := func(ts []*v1alpha1.Task) (out []string) {
		for _, t := range ts {
			out = append(out, t.GetMetadata().GetName())
		}
		return out
	}
	all, err := s.ListTasks(ctx, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(all); len(got) != 3 || got[0] != "three" || got[2] != "one" {
		t.Errorf("ListTasks(all) = %v, want newest first", got)
	}
	inA, _ := s.ListTasks(ctx, "a", 0, 0)
	if got := names(inA); len(got) != 2 || got[0] != "two" || got[1] != "one" {
		t.Errorf("ListTasks(a) = %v", got)
	}
	page, _ := s.ListTasks(ctx, "*", 1, 1)
	if got := names(page); len(got) != 1 || got[0] != "two" {
		t.Errorf("ListTasks(limit 1, offset 1) = %v", got)
	}
}

func TestWorkspacesAndModels(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, filepath.Join(t.TempDir(), "db.sqlite"))

	ws := &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "w"}, Spec: &v1alpha1.WorkspaceSpec{}}
	if err := s.SaveWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetWorkspace(ctx, "default", "w"); err != nil || got.GetMetadata().GetName() != "w" {
		t.Fatalf("GetWorkspace = %v, %v", got, err)
	}
	if l, _ := s.ListWorkspaces(ctx, "default"); len(l) != 1 {
		t.Errorf("ListWorkspaces len = %d", len(l))
	}
	if err := s.DeleteWorkspace(ctx, "default", "w"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetWorkspace(ctx, "default", "w"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetWorkspace after delete err = %v", err)
	}

	m := &v1alpha1.Model{Metadata: &v1alpha1.ObjectMeta{Name: "m", Atespace: "x"}, Spec: &v1alpha1.ModelSpec{Provider: "google", Model: "g"}}
	if err := s.SaveModel(ctx, m); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetModel(ctx, "x", "m"); err != nil || got.GetSpec().GetModel() != "g" {
		t.Fatalf("GetModel = %v, %v", got, err)
	}
	if l, _ := s.ListModels(ctx, "default"); len(l) != 0 {
		t.Errorf("ListModels(default) len = %d, want 0", len(l))
	}
	if err := s.DeleteModel(ctx, "x", "m"); err != nil {
		t.Fatal(err)
	}
}

// TestWatchSeesWritesFromAnotherHandle simulates two ax processes sharing the
// database: one watches, the other writes.
func TestWatchSeesWritesFromAnotherHandle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "db.sqlite")
	watcher := openTestStore(t, path)
	writer := openTestStore(t, path)

	if err := writer.SaveTask(ctx, task("default", "w")); err != nil {
		t.Fatal(err)
	}
	ch, closer, err := watcher.WatchTask(ctx, "default", "w")
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	if err := writer.UpdateTaskStatus(ctx, "default", "w", &v1alpha1.TaskStatus{Phase: "Running"}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-ch:
		if got.GetStatus().GetPhase() != "Running" {
			t.Errorf("watch event phase = %q, want Running", got.GetStatus().GetPhase())
		}
	case <-ctx.Done():
		t.Fatal("no watch event for a write from another handle")
	}

	// Closing the watcher ends the stream.
	_ = closer.Close()
	for range ch {
	}
}
