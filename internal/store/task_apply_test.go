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

package store_test

import (
	"testing"

	"github.com/google/ax/internal/lifecycle"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

func TestTaskRowLifecycleRoundTripAndStoredStatus(t *testing.T) {
	if got := (store.TaskKey{Name: "t1"}).Normalized(); got.Atespace != v1alpha1.DefaultAtespace || got.Name != "t1" {
		t.Fatalf("TaskKey.Normalized() = %+v", got)
	}

	// Non-AX states (WORKING, SUSPENDED, SUSPENDING) and Ready condition are
	// stripped by StoredStatus / SetLifecycleState while WorkspaceReady and
	// runner fields are preserved.
	var row store.TaskRow
	liveState := lifecycle.State{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Atespace: "payments", Name: "fix-bug"},
			Spec:     &v1alpha1.TaskSpec{SessionId: "sess-1"},
			Status: &v1alpha1.TaskStatus{
				State:            v1alpha1.TaskState_TASK_STATE_WORKING,
				Actor:            "fix-bug",
				ActorUid:         "live-uid-must-not-persist",
				AteActorTemplate: "ax-0123456789abcdef",
				Attempt:          1,
				Conditions: []*v1alpha1.Condition{
					{Type: v1alpha1.ConditionReady, Status: "True"},
					{Type: v1alpha1.ConditionWorkspaceReady, Status: "True"},
				},
			},
		},
		ActorUID:             "actor-uid-1",
		Runnable:             true,
		ActiveRunID:          "r-1",
		RunSeq:               1,
		RunnerTokenHash:      []byte{1, 2, 3},
		RunnerTokenConfirmed: true,
		LastAckedRunnerSeq:   4,
	}

	row.SetLifecycleState(liveState)
	st := row.Task.GetStatus()
	if st.GetState() != v1alpha1.TaskState_TASK_STATE_UNSPECIFIED || st.GetActor() != "" || st.GetActorUid() != "" {
		t.Fatalf("expected actor-derived state and actor identity stripped, got %+v", st)
	}
	if len(st.GetConditions()) != 1 || st.GetConditions()[0].GetType() != v1alpha1.ConditionWorkspaceReady {
		t.Fatalf("Conditions = %+v, want only WorkspaceReady", st.GetConditions())
	}

	// AX-owned states (QUEUED, COMPLETED, FAILED) keep their state, outcome,
	// and conditions while still scrubbing Actor and ActorUid.
	completedState := liveState.Clone()
	completedState.Runnable = false
	completedState.ActiveRunID = ""
	completedState.Task.Status.State = v1alpha1.TaskState_TASK_STATE_COMPLETED
	completedState.Task.Status.Outcome = &v1alpha1.TaskOutcome{
		Result:   v1alpha1.TaskResult_TASK_RESULT_SUCCEEDED,
		ExitCode: 0,
		Output:   "all tests passed",
	}
	row.SetLifecycleState(completedState)
	if row.Task.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_COMPLETED ||
		row.Task.GetStatus().GetOutcome().GetOutput() != "all tests passed" ||
		row.Task.GetStatus().GetActor() != "" || row.Task.GetStatus().GetActorUid() != "" {
		t.Fatalf("unexpected stored terminal status: %+v", row.Task.GetStatus())
	}

	cp := row.Clone()
	cp.RunnerTokenHash[0] = 99
	cp.Task.Metadata.Name = "mutated"
	if row.RunnerTokenHash[0] != 1 || row.Task.GetMetadata().GetName() != "fix-bug" {
		t.Fatal("TaskRow.Clone shared mutable state with original")
	}

	extracted := row.LifecycleState()
	if extracted.ActorUID != "actor-uid-1" || extracted.LastAckedRunnerSeq != 4 || extracted.Runnable {
		t.Fatalf("LifecycleState() = %+v", extracted)
	}

	// Nil Task.Status is initialized to an empty non-nil TaskStatus.
	var nilStatusRow store.TaskRow
	nilStatusRow.SetLifecycleState(lifecycle.State{Task: &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: "t2"}}})
	if nilStatusRow.Task.GetStatus() == nil {
		t.Fatal("expected SetLifecycleState to initialize nil Task.Status")
	}

	// Nil receiver helpers are safe no-ops / zero returns.
	var nilRow *store.TaskRow
	nilRow.SetLifecycleState(liveState)
	if nilRow.Clone() != nil || nilRow.LifecycleState().Task != nil {
		t.Fatal("nil TaskRow helpers must return zero/nil values")
	}

	// DeleteTaskRow is matched strictly by pointer identity.
	if store.DeleteTaskRow == nil || store.DeleteTaskRow.Clone() == store.DeleteTaskRow {
		t.Fatal("DeleteTaskRow clone must not match sentinel pointer identity")
	}
}
