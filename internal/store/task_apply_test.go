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

func TestTaskRowCloneIdentifiersAndStoredStatus(t *testing.T) {
	if got := (store.TaskKey{Name: "t1"}).Normalized(); got.Atespace != v1alpha1.DefaultAtespace || got.Name != "t1" {
		t.Fatalf("TaskKey.Normalized() = %+v", got)
	}

	// Non-AX states (WORKING, SUSPENDED, SUSPENDING) and Ready condition are
	// stripped by StoredStatus while WorkspaceReady and template metadata stay.
	liveStatus := &v1alpha1.TaskStatus{
		State:            v1alpha1.TaskState_TASK_STATE_WORKING,
		Actor:            "fix-bug",
		ActorUid:         "live-uid-must-not-persist",
		AteActorTemplate: "ax-0123456789abcdef",
		Attempt:          1,
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

	row := &store.TaskRow{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Atespace: "payments", Name: "fix-bug"},
			Spec:     &v1alpha1.TaskSpec{SessionId: "sess-1"},
			Status:   scrubbed,
		},
		Generation:           2,
		ActorUID:             "actor-uid-1",
		Runnable:             true,
		RunnerTokenHash:      []byte{1, 2, 3},
		RunnerTokenConfirmed: true,
		ClaimNonce:           "nonce-1",
		LastAckedRunnerSeq:   4,
	}
	if r1, r2 := row.NextRunID(), row.NextRunID(); r1 != "r-1" || r2 != "r-2" {
		t.Fatalf("NextRunID = (%q, %q), want (r-1, r-2)", r1, r2)
	}

	cp := row.Clone()
	cp.RunnerTokenHash[0] = 99
	cp.Task.Metadata.Name = "mutated"
	cp.Runnable = false
	if row.RunnerTokenHash[0] != 1 || row.Task.GetMetadata().GetName() != "fix-bug" || !row.Runnable {
		t.Fatal("TaskRow.Clone shared mutable state with original")
	}

	// Nil and empty-slice fields remain nil and isolated across Clone.
	empty := &store.TaskRow{RunnerTokenHash: []byte{}}
	emptyClone := empty.Clone()
	if emptyClone.Task != nil || emptyClone.RunnerTokenHash != nil {
		t.Fatalf("Clone of empty/nil fields produced non-nil pointers/slices: %+v", emptyClone)
	}
	emptyClone.Task = &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: "isolated"}}
	emptyClone.RunnerTokenHash = append(emptyClone.RunnerTokenHash, 7)
	if empty.Task != nil || len(empty.RunnerTokenHash) != 0 {
		t.Fatalf("mutating cloned nil/empty fields affected original: %+v", empty)
	}

	// Nil receiver helpers are safe.
	var nilRow *store.TaskRow
	if nilRow.Clone() != nil || nilRow.NextRunID() != "" {
		t.Fatal("nil TaskRow helpers must return zero/nil values")
	}

	// DeleteTaskRow is matched strictly by pointer identity.
	if store.DeleteTaskRow == nil || store.DeleteTaskRow.Clone() == store.DeleteTaskRow {
		t.Fatal("DeleteTaskRow clone must not match sentinel pointer identity")
	}
}
