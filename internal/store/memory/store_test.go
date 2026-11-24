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

package memory_test

import (
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/google/ax/internal/store"
	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

func TestListPagination(t *testing.T) {
	ctx := t.Context()
	s := memory.NewStore()
	for _, name := range []string{"c", "a", "b"} {
		if err := s.SaveWorkspace(ctx, &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: "ws-" + name, Atespace: "default"},
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveTask(ctx, &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "task-" + name, Atespace: "default"},
		}); err != nil {
			t.Fatal(err)
		}
	}

	wsNames := func(items []*v1alpha1.Workspace) []string {
		var out []string
		for _, ws := range items {
			out = append(out, ws.GetMetadata().GetName())
		}
		return out
	}
	taskNames := func(items []*v1alpha1.Task) []string {
		var out []string
		for _, task := range items {
			out = append(out, task.GetMetadata().GetName())
		}
		return out
	}

	page, err := s.ListWorkspaces(ctx, "default", 1, 1)
	if err != nil || !slices.Equal(wsNames(page), []string{"ws-b"}) {
		t.Fatalf("ListWorkspaces(limit=1, offset=1) = %v, %v; want [ws-b]", wsNames(page), err)
	}

	// Negative offset clamps to 0 and limit <= 0 applies default page limit.
	allWS, err := s.ListWorkspaces(ctx, "default", 0, -5)
	if err != nil || !slices.Equal(wsNames(allWS), []string{"ws-a", "ws-b", "ws-c"}) {
		t.Fatalf("ListWorkspaces(limit=0, offset=-5) = %v, %v; want [ws-a ws-b ws-c]", wsNames(allWS), err)
	}
	allTasks, err := s.ListTasks(ctx, "default", 0, -1)
	if err != nil || !slices.Equal(taskNames(allTasks), []string{"task-a", "task-b", "task-c"}) {
		t.Fatalf("ListTasks(limit=0, offset=-1) = %v, %v; want [task-a task-b task-c]", taskNames(allTasks), err)
	}

	empty, err := s.ListWorkspaces(ctx, "default", 10, 99)
	if err != nil || len(empty) != 0 {
		t.Fatalf("ListWorkspaces(offset=99) = %v, %v; want empty", wsNames(empty), err)
	}

	// Large limit + positive offset must not overflow int64 and panic.
	maxWS, err := s.ListWorkspaces(ctx, "default", math.MaxInt64, 1)
	if err != nil || !slices.Equal(wsNames(maxWS), []string{"ws-b", "ws-c"}) {
		t.Fatalf("ListWorkspaces(limit=MaxInt64, offset=1) = %v, %v; want [ws-b ws-c]", wsNames(maxWS), err)
	}
	maxTasks, err := s.ListTasks(ctx, "default", math.MaxInt64, 1)
	if err != nil || !slices.Equal(taskNames(maxTasks), []string{"task-b", "task-c"}) {
		t.Fatalf("ListTasks(limit=MaxInt64, offset=1) = %v, %v; want [task-b task-c]", taskNames(maxTasks), err)
	}
}

func TestUpdateWorkspaceStatus(t *testing.T) {
	ctx := t.Context()
	s := memory.NewStore()

	if err := s.UpdateWorkspaceStatus(ctx, "default", "missing", &v1alpha1.WorkspaceStatus{Phase: v1alpha1.WorkspacePhaseReady}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateWorkspaceStatus(missing) = %v, want ErrNotFound", err)
	}

	if err := s.SaveWorkspace(ctx, &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "ws-1", Atespace: "default"},
	}); err != nil {
		t.Fatal(err)
	}

	st := &v1alpha1.WorkspaceStatus{
		Phase:          v1alpha1.WorkspacePhaseFailed,
		LastSetupError: "clone failed",
	}
	if err := s.UpdateWorkspaceStatus(ctx, "default", "ws-1", st); err != nil {
		t.Fatalf("UpdateWorkspaceStatus: %v", err)
	}
	// Mutating caller's status after update must not mutate stored workspace.
	st.Phase = v1alpha1.WorkspacePhaseReady

	got, err := s.GetWorkspace(ctx, "default", "ws-1")
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if got.GetStatus().GetPhase() != v1alpha1.WorkspacePhaseFailed || got.GetStatus().GetLastSetupError() != "clone failed" {
		t.Fatalf("stored workspace status = %+v, want Failed / clone failed", got.GetStatus())
	}
}

func TestSaveAndUpdateTask(t *testing.T) {
	ctx := t.Context()
	s := memory.NewStore()

	if err := s.UpdateTaskStatus(ctx, "default", "missing", &v1alpha1.TaskStatus{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateTaskStatus(missing) = %v, want ErrNotFound", err)
	}

	// SaveTask preserves TASK_STATE_UNSPECIFIED rather than coercing to QUEUED.
	if err := s.SaveTask(ctx, &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "t1", Atespace: "default"},
	}); err != nil {
		t.Fatalf("SaveTask: %v", err)
	}
	got, err := s.GetTask(ctx, "default", "t1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_UNSPECIFIED {
		t.Fatalf("stored state = %v, want UNSPECIFIED", got.GetStatus().GetState())
	}

	st := &v1alpha1.TaskStatus{State: v1alpha1.TaskState_TASK_STATE_FAILED}
	if err := s.UpdateTaskStatus(ctx, "default", "t1", st); err != nil {
		t.Fatalf("UpdateTaskStatus: %v", err)
	}
	// Mutating caller's status after update must not mutate stored task.
	st.State = v1alpha1.TaskState_TASK_STATE_QUEUED

	got, err = s.GetTask(ctx, "default", "t1")
	if err != nil {
		t.Fatalf("GetTask after update: %v", err)
	}
	if got.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_FAILED {
		t.Fatalf("stored state after update = %v, want FAILED", got.GetStatus().GetState())
	}
}

// A task stays in the working queue until it or the task is deleted, and only
// a stored task can be queued.
func TestWorkingTasks(t *testing.T) {
	s := memory.NewStore()
	ctx := t.Context()
	if err := s.Queue(ctx, &v1alpha1.ObjectRef{Atespace: "default", Name: "missing"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Queue(missing) = %v, want ErrNotFound", err)
	}
	for _, name := range []string{"a", "b"} {
		if err := s.SaveTask(ctx, &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: name, Atespace: "default"}}); err != nil {
			t.Fatalf("SaveTask(%s): %v", name, err)
		}
		// Adding twice is not an error.
		for range 2 {
			if err := s.Queue(ctx, &v1alpha1.ObjectRef{Atespace: "default", Name: name}); err != nil {
				t.Fatalf("Queue(%s): %v", name, err)
			}
		}
	}
	check := func(what string, want ...string) {
		t.Helper()
		got, err := s.Claim(ctx)
		if err != nil {
			t.Fatalf("%s: Claim: %v", what, err)
		}
		var names []string
		for _, w := range got {
			if w.GetAtespace() != "default" {
				t.Errorf("%s: working task %v, want atespace default", what, w)
			}
			names = append(names, w.GetName())
		}
		if !slices.Equal(names, want) {
			t.Errorf("%s: working tasks = %v, want %v", what, names, want)
		}
	}
	check("added", "a", "b")

	// Saving a task again keeps it queued.
	if err := s.SaveTask(ctx, &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: "a", Atespace: "default"}}); err != nil {
		t.Fatalf("SaveTask(a): %v", err)
	}
	check("saved again", "a", "b")

	a := &v1alpha1.ObjectRef{Atespace: "default", Name: "a"}
	if err := s.Delete(ctx, a); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(ctx, a); err != nil {
		t.Fatalf("Delete again: %v", err)
	}
	if _, err := s.GetTask(ctx, "default", "a"); err != nil {
		t.Fatalf("GetTask(a) after Delete: %v", err)
	}
	check("mark deleted", "b")

	if err := s.DeleteTask(ctx, "default", "b"); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	check("task deleted")
}
