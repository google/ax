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
	"sync"
	"testing"
	"time"

	"github.com/google/ax/internal/store"
	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/types/known/timestamppb"
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

	dt := timestamppb.Now()
	if err := s.SetTaskDeleteTime(ctx, "default", "task-a", dt); err != nil {
		t.Fatalf("SetTaskDeleteTime: %v", err)
	}
	if got, err := s.GetTask(ctx, "default", "task-a"); err != nil || got.GetMetadata().GetDeleteTime() == nil {
		t.Fatalf("GetTask(task-a) delete_time = %v, err = %v; want non-nil", got.GetMetadata().GetDeleteTime(), err)
	}
	if err := s.SetTaskDeleteTime(ctx, "default", "task-a", nil); err != nil {
		t.Fatalf("SetTaskDeleteTime(nil): %v", err)
	}
	if got, err := s.GetTask(ctx, "default", "task-a"); err != nil || got.GetMetadata().GetDeleteTime() != nil {
		t.Fatalf("GetTask(task-a) delete_time after clear = %v, err = %v; want nil", got.GetMetadata().GetDeleteTime(), err)
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

func TestTaskApplier(t *testing.T) {
	ctx := t.Context()
	s := memory.NewStore()
	key := store.TaskKey{Name: "t-apply"} // empty Atespace normalizes to "default"

	// Missing row: GetTaskRow returns ErrNotFound; Apply nil / DeleteTaskRow are no-ops.
	if _, err := s.GetTaskRow(ctx, key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetTaskRow(missing) = %v, want ErrNotFound", err)
	}
	if got, err := s.Apply(ctx, key, func(current *store.TaskRow, _ time.Time) (*store.TaskRow, error) {
		if current != nil {
			t.Fatalf("expected nil current on missing row, got %+v", current)
		}
		return nil, nil
	}); err != nil || got != nil {
		t.Fatalf("Apply(missing -> nil) = (%v, %v), want (nil, nil)", got, err)
	}
	if got, err := s.Apply(ctx, key, func(*store.TaskRow, time.Time) (*store.TaskRow, error) {
		return store.DeleteTaskRow, nil
	}); err != nil || got != nil {
		t.Fatalf("Apply(missing -> DeleteTaskRow) = (%v, %v), want (nil, nil)", got, err)
	}

	// Returning a row with nil Task is rejected without mutating storage.
	if _, err := s.Apply(ctx, key, func(*store.TaskRow, time.Time) (*store.TaskRow, error) {
		return &store.TaskRow{}, nil
	}); err == nil {
		t.Fatal("expected error when ApplyFunc returns TaskRow with nil Task")
	}

	// Concurrent insert race: exactly one creator sees current == nil.
	errExists := errors.New("already exists")
	const racers = 8
	var wg sync.WaitGroup
	errs := make(chan error, racers)
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Apply(ctx, key, func(current *store.TaskRow, _ time.Time) (*store.TaskRow, error) {
				if current != nil {
					return nil, errExists
				}
				return &store.TaskRow{
					Task: &v1alpha1.Task{
						Spec: &v1alpha1.TaskSpec{SessionId: "sess-1"},
						Status: &v1alpha1.TaskStatus{
							State:    v1alpha1.TaskState_TASK_STATE_WORKING,
							Actor:    "live-actor",
							ActorUid: "live-uid",
							Conditions: []*v1alpha1.Condition{
								{Type: v1alpha1.ConditionReady, Status: "True"},
								{Type: v1alpha1.ConditionWorkspaceReady, Status: "True"},
							},
						},
					},
				}, nil
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	var wins, conflicts int
	for err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, errExists):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent Apply error: %v", err)
		}
	}
	if wins != 1 || conflicts != racers-1 {
		t.Fatalf("concurrent insert wins=%d conflicts=%d, want 1 and %d", wins, conflicts, racers-1)
	}

	inserted, err := s.GetTaskRow(ctx, key)
	if err != nil {
		t.Fatalf("GetTaskRow(inserted): %v", err)
	}
	if inserted.Generation != 1 || inserted.Task.GetStatus().GetGeneration() != 1 {
		t.Fatalf("Generation = (%d, %d), want (1, 1)", inserted.Generation, inserted.Task.GetStatus().GetGeneration())
	}
	if inserted.Task.GetMetadata().GetAtespace() != v1alpha1.DefaultAtespace || inserted.Task.GetMetadata().GetName() != "t-apply" {
		t.Fatalf("Metadata = %+v, want default/t-apply", inserted.Task.GetMetadata())
	}
	if inserted.Task.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_UNSPECIFIED ||
		inserted.Task.GetStatus().GetActor() != "" || inserted.Task.GetStatus().GetActorUid() != "" ||
		len(inserted.Task.GetStatus().GetConditions()) != 1 {
		t.Fatalf("StoredStatus did not scrub live fields on insert: %+v", inserted.Task.GetStatus())
	}

	// Returning current (even if mutated in place) leaves storage unchanged.
	noopRow, err := s.Apply(ctx, key, func(current *store.TaskRow, _ time.Time) (*store.TaskRow, error) {
		current.Task.Spec.SessionId = "mutated-in-place"
		return current, nil
	})
	if err != nil || noopRow.Task.GetSpec().GetSessionId() != "sess-1" || noopRow.Generation != 1 {
		t.Fatalf("Apply(return current) = (%+v, %v), want unchanged sessionId=sess-1 Gen=1", noopRow, err)
	}

	// Update advances Generation to 2 and persists terminal state + runner fields.
	updated, err := s.Apply(ctx, key, func(current *store.TaskRow, _ time.Time) (*store.TaskRow, error) {
		next := current.Clone()
		next.Task.Status.State = v1alpha1.TaskState_TASK_STATE_COMPLETED
		next.Task.Status.Outcome = &v1alpha1.TaskOutcome{
			Result: v1alpha1.TaskResult_TASK_RESULT_SUCCEEDED,
			Output: "done",
		}
		return next, nil
	})
	if err != nil {
		t.Fatalf("Apply(update): %v", err)
	}
	if updated.Generation != 2 ||
		updated.Task.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_COMPLETED ||
		updated.Task.GetStatus().GetOutcome().GetOutput() != "done" {
		t.Fatalf("updated row = %+v, status = %+v", updated, updated.Task.GetStatus())
	}

	// Clone of DeleteTaskRow does not match sentinel pointer identity and must not delete.
	if _, err := s.Apply(ctx, key, func(*store.TaskRow, time.Time) (*store.TaskRow, error) {
		return store.DeleteTaskRow.Clone(), nil
	}); err == nil {
		t.Fatal("expected error when returning DeleteTaskRow.Clone()")
	}

	// Returning DeleteTaskRow itself removes the row.
	deleted, err := s.Apply(ctx, key, func(*store.TaskRow, time.Time) (*store.TaskRow, error) {
		return store.DeleteTaskRow, nil
	})
	if err != nil || deleted != nil {
		t.Fatalf("Apply(DeleteTaskRow) = (%v, %v), want (nil, nil)", deleted, err)
	}
	if _, err := s.GetTaskRow(ctx, key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetTaskRow after delete = %v, want ErrNotFound", err)
	}
	if _, err := s.GetTask(ctx, v1alpha1.DefaultAtespace, "t-apply"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetTask after delete = %v, want ErrNotFound", err)
	}
}
