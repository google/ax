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

	"github.com/google/ax/internal/store"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

func TestTaskVersionCloneIdentifiersAndStoredStatus(t *testing.T) {
	if got := (store.TaskKey{Name: "t1"}).Normalized(); got.Atespace != v1alpha1.DefaultAtespace || got.Name != "t1" {
		t.Fatalf("TaskKey.Normalized() = %+v", got)
	}

	// Non-AX states (WORKING, SUSPENDED, SUSPENDING) and Ready condition are
	// stripped by StoredStatus while WorkspaceReady and template metadata stay.
	liveStatus := &v1alpha1.TaskStatus{
		State:       v1alpha1.TaskState_TASK_STATE_WORKING,
		Actor:       "fix-bug",
		ActorUid:    "live-uid-must-not-persist",
		AteTemplate: &v1alpha1.ObjectRef{Name: "ax-0123456789abcdef", Atespace: "default"},
		Attempt:     1,
		Conditions: []*v1alpha1.Condition{
			{Type: v1alpha1.ConditionReady, Status: "True"},
			{Type: v1alpha1.ConditionWorkspaceReady, Status: "True"},
		},
	}
	scrubbed := store.StoredStatus(liveStatus)
	if scrubbed.GetState() != v1alpha1.TaskState_TASK_STATE_UNSPECIFIED || scrubbed.GetActor() != "" || scrubbed.GetActorUid() != "" {
		t.Fatalf("expected actor-derived state and actor identity stripped, got %+v", scrubbed)
	}
	if len(scrubbed.GetConditions()) != 1 || scrubbed.GetConditions()[0].GetType() != v1alpha1.ConditionWorkspaceReady {
		t.Fatalf("Conditions = %+v, want only WorkspaceReady", scrubbed.GetConditions())
	}
	if liveStatus.GetActor() != "fix-bug" || len(liveStatus.GetConditions()) != 2 {
		t.Fatalf("StoredStatus mutated input: %+v", liveStatus)
	}
	if store.StoredStatus(nil) == nil {
		t.Fatal("StoredStatus(nil) = nil, want non-nil empty TaskStatus")
	}

	// AX-owned states (QUEUED, COMPLETED, FAILED) keep their state, outcome,
	// and conditions while still scrubbing Actor and ActorUid.
	terminalStatus := store.StoredStatus(&v1alpha1.TaskStatus{
		State:    v1alpha1.TaskState_TASK_STATE_COMPLETED,
		Actor:    "fix-bug",
		ActorUid: "uid-1",
		Outcome: &v1alpha1.TaskOutcome{
			Result:   v1alpha1.TaskResult_TASK_RESULT_SUCCEEDED,
			ExitCode: 0,
			Output:   "all tests passed",
		},
		Conditions: []*v1alpha1.Condition{{Type: v1alpha1.ConditionReady, Status: "False"}},
	})
	if terminalStatus.GetState() != v1alpha1.TaskState_TASK_STATE_COMPLETED ||
		terminalStatus.GetOutcome().GetOutput() != "all tests passed" ||
		len(terminalStatus.GetConditions()) != 1 ||
		terminalStatus.GetActor() != "" || terminalStatus.GetActorUid() != "" {
		t.Fatalf("unexpected stored terminal status: %+v", terminalStatus)
	}

	row := &store.TaskVersion{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Atespace: "payments", Name: "fix-bug"},
			Spec:     &v1alpha1.TaskSpec{SessionId: "sess-1"},
			Status:   scrubbed,
		},
		Generation: 2,
	}

	cp := row.Clone()
	cp.Task.Metadata.Name = "mutated"
	cp.Generation = 3
	if row.Task.GetMetadata().GetName() != "fix-bug" || row.Generation != 2 {
		t.Fatal("TaskVersion.Clone shared mutable state with original")
	}

	// A nil Task stays nil and isolated across Clone.
	empty := &store.TaskVersion{}
	emptyClone := empty.Clone()
	if emptyClone.Task != nil {
		t.Fatalf("Clone of nil Task produced a non-nil Task: %+v", emptyClone)
	}
	emptyClone.Task = &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: "isolated"}}
	if empty.Task != nil {
		t.Fatalf("mutating cloned nil Task affected original: %+v", empty)
	}

	// Nil receiver helpers are safe.
	var nilRow *store.TaskVersion
	if nilRow.Clone() != nil {
		t.Fatal("nil TaskVersion helpers must return zero/nil values")
	}

	// DeleteTaskVersion is matched strictly by pointer identity.
	if store.DeleteTaskVersion == nil || store.DeleteTaskVersion.Clone() == store.DeleteTaskVersion {
		t.Fatal("DeleteTaskVersion clone must not match sentinel pointer identity")
	}
}
