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

package lifecycle_test

import (
	"testing"

	"github.com/google/ax/internal/lifecycle"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

func TestStateCloneAndIdentifiers(t *testing.T) {
	s := lifecycle.State{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "t1"},
			Status:   &v1alpha1.TaskStatus{State: v1alpha1.TaskState_TASK_STATE_QUEUED},
		},
		ActorUID:             "actor-uid-1",
		Runnable:             true,
		RunnerTokenHash:      []byte{1, 2, 3},
		RunnerTokenConfirmed: true,
		ClaimNonce:           "nonce-1",
		LastAckedRunnerSeq:   2,
	}
	r1 := s.NextRunID()
	r2 := s.NextRunID()
	if r1 != "r-1" || r2 != "r-2" {
		t.Fatalf("NextRunID = (%q, %q), want (r-1, r-2)", r1, r2)
	}

	cp := s.Clone()
	cp.Task.Metadata.Name = "mutated"
	cp.RunnerTokenHash[0] = 99
	cp.Runnable = false

	if s.Task.Metadata.Name != "t1" || s.RunnerTokenHash[0] != 1 || !s.Runnable {
		t.Fatalf("Clone shared mutable state with original: %+v", s)
	}

	// Nil and empty-slice fields remain nil and isolated across Clone.
	empty := lifecycle.State{
		RunnerTokenHash: []byte{},
	}
	emptyClone := empty.Clone()
	if emptyClone.Task != nil || emptyClone.RunnerTokenHash != nil {
		t.Fatalf("Clone of empty/nil fields produced non-nil pointers/slices: %+v", emptyClone)
	}
	emptyClone.Task = &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: "isolated"}}
	emptyClone.RunnerTokenHash = append(emptyClone.RunnerTokenHash, 7)
	if empty.Task != nil || len(empty.RunnerTokenHash) != 0 {
		t.Fatalf("mutating cloned nil/empty fields affected original: %+v", empty)
	}
}

func TestIsTerminalAndInputs(t *testing.T) {
	for _, tc := range []struct {
		state v1alpha1.TaskState
		want  bool
	}{
		{v1alpha1.TaskState_TASK_STATE_UNSPECIFIED, false},
		{v1alpha1.TaskState_TASK_STATE_QUEUED, false},
		{v1alpha1.TaskState_TASK_STATE_WORKING, false},
		{v1alpha1.TaskState_TASK_STATE_IDLE, false},
		{v1alpha1.TaskState_TASK_STATE_SUSPENDED, false},
		{v1alpha1.TaskState_TASK_STATE_SUSPENDING, false},
		{v1alpha1.TaskState_TASK_STATE_COMPLETED, true},
		{v1alpha1.TaskState_TASK_STATE_FAILED, true},
		{v1alpha1.TaskState_TASK_STATE_CANCELLED, true},
	} {
		if got := lifecycle.IsTerminal(tc.state); got != tc.want {
			t.Errorf("IsTerminal(%v) = %v, want %v", tc.state, got, tc.want)
		}
	}

	inputs := []lifecycle.Input{
		&lifecycle.CreateInput{},
		&lifecycle.SuspendInput{},
		&lifecycle.ResumeInput{},
		&lifecycle.DeleteInput{},
	}
	if len(inputs) != 4 {
		t.Fatalf("inputs count = %d, want 4", len(inputs))
	}
}
