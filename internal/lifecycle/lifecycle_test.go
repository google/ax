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
	"time"

	"github.com/google/ax/internal/lifecycle"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestStateCloneAndIdentifiers(t *testing.T) {
	t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	deadline := t0.Add(lifecycle.ClaimWatchdogTimeout)
	resumedAt := t0.Add(-time.Minute)

	s := lifecycle.State{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "t1"},
			Status:   &v1alpha1.TaskStatus{State: v1alpha1.TaskState_TASK_STATE_QUEUED},
		},
		RunnerTokenHash:    []byte{1, 2, 3},
		LastAckedRunnerSeq: 2,
		ClaimDeadline:      &deadline,
		LastResumedAt:      &resumedAt,
	}
	a1 := s.NextAction(lifecycle.ActionEnsureTemplate, t0)
	s.Actions = []lifecycle.Action{a1}
	r1 := s.NextRunID()
	if a1.ID != "a-1" || r1 != "r-1" {
		t.Fatalf("NextAction=%q NextRunID=%q, want a-1 and r-1", a1.ID, r1)
	}

	cp := s.Clone()
	cp.Task.Metadata.Name = "mutated"
	cp.Actions[0].Tag = "mutated-tag"
	cp.RunnerTokenHash[0] = 99
	*cp.ClaimDeadline = t0
	*cp.LastResumedAt = t0

	if s.Task.Metadata.Name != "t1" || s.Actions[0].Tag != "" || s.RunnerTokenHash[0] != 1 ||
		!s.ClaimDeadline.Equal(deadline) || !s.LastResumedAt.Equal(resumedAt) {
		t.Fatalf("Clone shared mutable state with original: %+v", s)
	}

	// Nil and empty-slice fields remain nil and isolated across Clone.
	empty := lifecycle.State{
		Actions:         []lifecycle.Action{},
		RunnerTokenHash: []byte{},
	}
	emptyClone := empty.Clone()
	if emptyClone.Task != nil || emptyClone.Actions != nil || emptyClone.RunnerTokenHash != nil ||
		emptyClone.ClaimDeadline != nil || emptyClone.LastResumedAt != nil {
		t.Fatalf("Clone of empty/nil fields produced non-nil pointers/slices: %+v", emptyClone)
	}
	emptyClone.Task = &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: "isolated"}}
	emptyClone.Actions = append(emptyClone.Actions, lifecycle.Action{ID: "a-99", Kind: lifecycle.ActionResumeActor})
	emptyClone.RunnerTokenHash = append(emptyClone.RunnerTokenHash, 7)
	emptyClone.ClaimDeadline = &deadline
	emptyClone.LastResumedAt = &resumedAt
	if empty.Task != nil || len(empty.Actions) != 0 || len(empty.RunnerTokenHash) != 0 ||
		empty.ClaimDeadline != nil || empty.LastResumedAt != nil {
		t.Fatalf("mutating cloned nil/empty fields affected original: %+v", empty)
	}
}

func TestNextReconcileAt(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	earlier := now.Add(-15 * time.Second)
	later := now.Add(15 * time.Second)
	deadline := now.Add(lifecycle.ClaimWatchdogTimeout)

	// 1. Pending action due immediately, clamped when NotBefore is in the past, or gated by future NotBefore.
	dueNow := lifecycle.NextReconcileAt(lifecycle.State{
		Actions: []lifecycle.Action{{ID: "a-1", Kind: lifecycle.ActionResumeActor}},
	}, now)
	if dueNow == nil || !dueNow.Equal(now) {
		t.Fatalf("NextReconcileAt(immediate action) = %v, want %v", dueNow, now)
	}
	duePast := lifecycle.NextReconcileAt(lifecycle.State{
		Actions: []lifecycle.Action{{ID: "a-1", Kind: lifecycle.ActionResumeActor, NotBefore: earlier}},
	}, now)
	if duePast == nil || !duePast.Equal(now) {
		t.Fatalf("NextReconcileAt(past NotBefore) = %v, want %v", duePast, now)
	}
	dueLater := lifecycle.NextReconcileAt(lifecycle.State{
		Actions: []lifecycle.Action{{ID: "a-1", Kind: lifecycle.ActionResumeActor, NotBefore: later}},
	}, now)
	if dueLater == nil || !dueLater.Equal(later) {
		t.Fatalf("NextReconcileAt(NotBefore) = %v, want %v", dueLater, later)
	}

	// 2. QUEUED with non-zero ClaimDeadline is due at ClaimDeadline, unless zero or deleting.
	queued := lifecycle.State{
		Task:          &v1alpha1.Task{Status: &v1alpha1.TaskStatus{State: v1alpha1.TaskState_TASK_STATE_QUEUED}},
		ClaimDeadline: &deadline,
	}
	if due := lifecycle.NextReconcileAt(queued, now); due == nil || !due.Equal(deadline) {
		t.Fatalf("NextReconcileAt(QUEUED watchdog) = %v, want %v", due, deadline)
	}
	var zeroTime time.Time
	queuedZeroDeadline := queued.Clone()
	queuedZeroDeadline.ClaimDeadline = &zeroTime
	if due := lifecycle.NextReconcileAt(queuedZeroDeadline, now); due != nil {
		t.Fatalf("NextReconcileAt(QUEUED zero ClaimDeadline) = %v, want nil", due)
	}
	deleting := queued.Clone()
	deleting.Task.Metadata = &v1alpha1.ObjectMeta{DeleteTime: timestamppb.New(now)}
	if due := lifecycle.NextReconcileAt(deleting, now); due != nil {
		t.Fatalf("NextReconcileAt(deleting without actions) = %v, want nil", due)
	}

	// 3. Steady-state WORKING / terminal tasks return nil, unless a post-completion action is still queued.
	working := lifecycle.State{
		Task: &v1alpha1.Task{Status: &v1alpha1.TaskStatus{State: v1alpha1.TaskState_TASK_STATE_WORKING}},
	}
	if due := lifecycle.NextReconcileAt(working, now); due != nil {
		t.Fatalf("NextReconcileAt(WORKING) = %v, want nil", due)
	}
	terminalParking := lifecycle.State{
		Task: &v1alpha1.Task{Status: &v1alpha1.TaskStatus{State: v1alpha1.TaskState_TASK_STATE_COMPLETED}},
		Actions: []lifecycle.Action{{
			ID:            "a-2",
			Kind:          lifecycle.ActionSuspendActorSync,
			SuspendReason: lifecycle.SuspendReasonTerminal,
		}},
	}
	if due := lifecycle.NextReconcileAt(terminalParking, now); due == nil || !due.Equal(now) {
		t.Fatalf("NextReconcileAt(COMPLETED with SUSPEND_ACTOR_SYNC) = %v, want %v", due, now)
	}
	terminalParked := terminalParking.Clone()
	terminalParked.Actions = nil
	if due := lifecycle.NextReconcileAt(terminalParked, now); due != nil {
		t.Fatalf("NextReconcileAt(COMPLETED without actions) = %v, want nil", due)
	}
	if !lifecycle.IsTerminal(v1alpha1.TaskState_TASK_STATE_COMPLETED) ||
		lifecycle.IsTerminal(v1alpha1.TaskState_TASK_STATE_IDLE) {
		t.Fatal("unexpected IsTerminal classification")
	}

	// Verify all canonical pointer variants satisfy lifecycle.Input.
	inputs := []lifecycle.Input{
		&lifecycle.CreateInput{},
		&lifecycle.SuspendInput{},
		&lifecycle.ResumeInput{},
		&lifecycle.DeleteInput{},
		&lifecycle.ActionCompletedInput{},
		&lifecycle.ActionFailedInput{},
		&lifecycle.CapacityUnavailableInput{},
		&lifecycle.ObservedActorInput{},
		&lifecycle.HeartbeatLostInput{},
		&lifecycle.TimerExpiredInput{},
	}
	if len(inputs) != 10 {
		t.Fatalf("inputs count = %d, want 10", len(inputs))
	}
}
