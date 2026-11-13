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
	"errors"
	"testing"
	"time"

	"github.com/google/ax/internal/lifecycle"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestCreateTask(t *testing.T) {
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

	t.Run("default starts QUEUED with attempt 1", func(t *testing.T) {
		st := &v1alpha1.TaskStatus{}
		spec := &v1alpha1.TaskSpec{ParentTaskId: "parent-1"}
		op, err := lifecycle.CreateTask(spec, st, t0)
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		if op != lifecycle.SubstrateOpCreateActor {
			t.Errorf("op = %q, want %q", op, lifecycle.SubstrateOpCreateActor)
		}
		if st.GetState() != v1alpha1.TaskState_TASK_STATE_QUEUED {
			t.Errorf("state = %v, want QUEUED", st.GetState())
		}
		if st.GetAttempt() != 1 {
			t.Errorf("attempt = %d, want 1", st.GetAttempt())
		}
		if st.GetParentTaskId() != "parent-1" {
			t.Errorf("parent_task_id = %q, want parent-1", st.GetParentTaskId())
		}
		if st.GetPhase() != v1alpha1.PhasePending {
			t.Errorf("phase = %q, want %q", st.GetPhase(), v1alpha1.PhasePending)
		}
	})

	t.Run("spec.suspend starts SUSPENDED with ADMIN_SUSPENDED", func(t *testing.T) {
		st := &v1alpha1.TaskStatus{}
		spec := &v1alpha1.TaskSpec{Suspend: true}
		op, err := lifecycle.CreateTask(spec, st, t0)
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		if op != lifecycle.SubstrateOpSuspendActor {
			t.Errorf("op = %q, want %q", op, lifecycle.SubstrateOpSuspendActor)
		}
		if st.GetState() != v1alpha1.TaskState_TASK_STATE_SUSPENDED {
			t.Errorf("state = %v, want SUSPENDED", st.GetState())
		}
		if st.GetStatusDetail().GetReason() != lifecycle.ReasonAdminSuspended {
			t.Errorf("reason = %q, want %q", st.GetStatusDetail().GetReason(), lifecycle.ReasonAdminSuspended)
		}
		if st.GetAttempt() != 1 {
			t.Errorf("attempt = %d, want 1", st.GetAttempt())
		}
		if st.GetPhase() != v1alpha1.PhaseSuspended {
			t.Errorf("phase = %q, want %q", st.GetPhase(), v1alpha1.PhaseSuspended)
		}
	})
}

func TestActiveLifecycleTransitions(t *testing.T) {
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	st := &v1alpha1.TaskStatus{}
	spec := &v1alpha1.TaskSpec{}

	if _, err := lifecycle.CreateTask(spec, st, t0); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// QUEUED -> WORKING
	t1 := t0.Add(5 * time.Second)
	if _, err := lifecycle.CommandStarted(st, t1); err != nil {
		t.Fatalf("CommandStarted: %v", err)
	}
	if st.GetState() != v1alpha1.TaskState_TASK_STATE_WORKING || st.GetPhase() != v1alpha1.PhaseRunning {
		t.Fatalf("after CommandStarted: state=%v phase=%q", st.GetState(), st.GetPhase())
	}
	if !st.GetStartTime().AsTime().Equal(t1) {
		t.Errorf("start_time = %v, want %v", st.GetStartTime().AsTime(), t1)
	}

	// WORKING -> SUSPENDING -> IDLE (IdleNotice accrues working_duration = 10s)
	t2 := t1.Add(10 * time.Second)
	op, err := lifecycle.IdleNotice(st, t2)
	if err != nil {
		t.Fatalf("IdleNotice: %v", err)
	}
	if op != lifecycle.SubstrateOpSuspendActor {
		t.Errorf("IdleNotice op = %q, want %q", op, lifecycle.SubstrateOpSuspendActor)
	}
	if st.GetState() != v1alpha1.TaskState_TASK_STATE_IDLE || st.GetPhase() != v1alpha1.PhaseIdle {
		t.Errorf("after IdleNotice: state=%v phase=%q", st.GetState(), st.GetPhase())
	}
	if st.GetStatusDetail().GetReason() != lifecycle.ReasonIdleTimeout {
		t.Errorf("IdleNotice reason = %q, want %q", st.GetStatusDetail().GetReason(), lifecycle.ReasonIdleTimeout)
	}
	if got := st.GetWorkingDuration().AsDuration(); got != 10*time.Second {
		t.Errorf("working_duration = %v, want 10s", got)
	}

	// IDLE ->SUSPENDED (SuspendTask)
	t3 := t2.Add(5 * time.Second)
	if _, err := lifecycle.SuspendTask(st, t3); err != nil {
		t.Fatalf("SuspendTask from IDLE: %v", err)
	}
	if st.GetState() != v1alpha1.TaskState_TASK_STATE_SUSPENDED || st.GetStatusDetail().GetReason() != lifecycle.ReasonAdminSuspended {
		t.Errorf("after SuspendTask: state=%v detail=%v", st.GetState(), st.GetStatusDetail())
	}
	// Working duration did not increase while IDLE.
	if got := st.GetWorkingDuration().AsDuration(); got != 10*time.Second {
		t.Errorf("working_duration after IDLE suspend = %v, want 10s", got)
	}

	// SUSPENDED -> QUEUED -> WORKING (ResumeTask)
	t4 := t3.Add(5 * time.Second)
	op, err = lifecycle.ResumeTask(st, t4)
	if err != nil {
		t.Fatalf("ResumeTask: %v", err)
	}
	if op != lifecycle.SubstrateOpResumeActor {
		t.Errorf("ResumeTask op = %q, want %q", op, lifecycle.SubstrateOpResumeActor)
	}
	if st.GetState() != v1alpha1.TaskState_TASK_STATE_WORKING || st.GetPhase() != v1alpha1.PhaseRunning {
		t.Errorf("after ResumeTask: state=%v phase=%q", st.GetState(), st.GetPhase())
	}
	if st.GetStatusDetail() != nil {
		t.Errorf("expected StatusDetail cleared on ResumeTask, got %v", st.GetStatusDetail())
	}

	// WORKING -> SUSPENDING (DEFERRED) -> QUEUED (Eviction accrues another 20s -> 30s total)
	t5 := t4.Add(20 * time.Second)
	if _, err := lifecycle.Eviction(st, t5); err != nil {
		t.Fatalf("Eviction: %v", err)
	}
	if st.GetState() != v1alpha1.TaskState_TASK_STATE_QUEUED || st.GetStatusDetail().GetReason() != lifecycle.ReasonDeferred {
		t.Errorf("after Eviction: state=%v detail=%v", st.GetState(), st.GetStatusDetail())
	}
	if got := st.GetWorkingDuration().AsDuration(); got != 30*time.Second {
		t.Errorf("working_duration after Eviction = %v, want 30s", got)
	}
}

func TestCommandExited_SuccessAndInPlaceRetry(t *testing.T) {
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	spec := &v1alpha1.TaskSpec{
		Retry: &v1alpha1.RetryConfig{MaxRetries: 2},
	}
	st := &v1alpha1.TaskStatus{}
	_, _ = lifecycle.CreateTask(spec, st, t0)
	_, _ = lifecycle.CommandStarted(st, t0)

	// Attempt 1 fails (1 <= 2): stays WORKING, attempt becomes 2
	t1 := t0.Add(5 * time.Second)
	op, err := lifecycle.CommandExited(spec, st, 1, "fail 1", nil, t1)
	if err != nil {
		t.Fatalf("CommandExited attempt 1: %v", err)
	}
	if op != lifecycle.SubstrateOpNone {
		t.Errorf("in-place retry op = %q, want none", op)
	}
	if st.GetState() != v1alpha1.TaskState_TASK_STATE_WORKING || st.GetAttempt() != 2 {
		t.Fatalf("after retry 1: state=%v attempt=%d, want WORKING attempt=2", st.GetState(), st.GetAttempt())
	}

	// Attempt 2 succeeds with exit code 0 -> COMPLETED
	structured, _ := structpb.NewStruct(map[string]any{"score": 1.0})
	t2 := t1.Add(7 * time.Second)
	op, err = lifecycle.CommandExited(spec, st, 0, "done", structured, t2)
	if err != nil {
		t.Fatalf("CommandExited exit 0: %v", err)
	}
	if op != lifecycle.SubstrateOpSuspendActor {
		t.Errorf("exit 0 op = %q, want SuspendActor", op)
	}
	if st.GetState() != v1alpha1.TaskState_TASK_STATE_COMPLETED || st.GetPhase() != v1alpha1.PhaseCompleted {
		t.Errorf("state=%v phase=%q, want COMPLETED/Completed", st.GetState(), st.GetPhase())
	}
	if st.GetOutcome().GetResult() != v1alpha1.TaskResult_TASK_RESULT_SUCCEEDED ||
		st.GetOutcome().GetExitCode() != 0 ||
		st.GetOutcome().GetOutput() != "done" ||
		st.GetOutcome().GetStructuredOutput().GetFields()["score"].GetNumberValue() != 1.0 {
		t.Errorf("unexpected outcome: %+v", st.GetOutcome())
	}
	if got := st.GetWorkingDuration().AsDuration(); got != 12*time.Second {
		t.Errorf("working_duration = %v, want 12s", got)
	}
}

func TestCommandExited_ExhaustsRetries(t *testing.T) {
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	spec := &v1alpha1.TaskSpec{
		Retry: &v1alpha1.RetryConfig{MaxRetries: 1},
	}
	st := &v1alpha1.TaskStatus{}
	_, _ = lifecycle.CreateTask(spec, st, t0)
	_, _ = lifecycle.CommandStarted(st, t0)

	// Attempt 1 fails (1 <= 1) -> attempt 2, WORKING
	if _, err := lifecycle.CommandExited(spec, st, 2, "err1", nil, t0.Add(time.Second)); err != nil {
		t.Fatalf("attempt 1: %v", err)
	}
	if st.GetState() != v1alpha1.TaskState_TASK_STATE_WORKING || st.GetAttempt() != 2 {
		t.Fatalf("expected WORKING attempt 2, got %v %d", st.GetState(), st.GetAttempt())
	}

	// Attempt 2 fails (2 > 1) -> FAILED (COMMAND_FAILED)
	op, err := lifecycle.CommandExited(spec, st, 42, "fatal", nil, t0.Add(2*time.Second))
	if err != nil {
		t.Fatalf("attempt 2: %v", err)
	}
	if op != lifecycle.SubstrateOpSuspendActor {
		t.Errorf("op = %q, want SuspendActor", op)
	}
	if st.GetState() != v1alpha1.TaskState_TASK_STATE_FAILED || st.GetPhase() != v1alpha1.PhaseFailed {
		t.Errorf("state=%v phase=%q, want FAILED/Failed", st.GetState(), st.GetPhase())
	}
	if st.GetStatusDetail().GetReason() != lifecycle.ReasonCommandFailed {
		t.Errorf("reason = %q, want %q", st.GetStatusDetail().GetReason(), lifecycle.ReasonCommandFailed)
	}
	if st.GetOutcome().GetResult() != v1alpha1.TaskResult_TASK_RESULT_FAILED || st.GetOutcome().GetExitCode() != 42 {
		t.Errorf("outcome = %+v, want FAILED exit_code=42", st.GetOutcome())
	}
}

func TestActorCrashed_RetryAndFail(t *testing.T) {
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	spec := &v1alpha1.TaskSpec{
		Retry: &v1alpha1.RetryConfig{MaxRetries: 1},
	}
	st := &v1alpha1.TaskStatus{}
	_, _ = lifecycle.CreateTask(spec, st, t0)
	_, _ = lifecycle.CommandStarted(st, t0)

	// First crash (attempt 1 <= 1): QUEUED with SANDBOX_CRASHED and RevertActor
	op, err := lifecycle.ActorCrashed(spec, st, "OOMKilled", t0.Add(3*time.Second))
	if err != nil {
		t.Fatalf("ActorCrashed 1: %v", err)
	}
	if op != lifecycle.SubstrateOpRevertActor {
		t.Errorf("op = %q, want RevertActor", op)
	}
	if st.GetState() != v1alpha1.TaskState_TASK_STATE_QUEUED || st.GetAttempt() != 2 {
		t.Fatalf("state=%v attempt=%d, want QUEUED attempt=2", st.GetState(), st.GetAttempt())
	}
	if st.GetStatusDetail().GetReason() != lifecycle.ReasonSandboxCrashed {
		t.Errorf("reason = %q, want SANDBOX_CRASHED", st.GetStatusDetail().GetReason())
	}

	// Resume and crash again (attempt 2 > 1): FAILED with SANDBOX_CRASHED
	_, _ = lifecycle.CommandStarted(st, t0.Add(4*time.Second))
	op, err = lifecycle.ActorCrashed(spec, st, "OOMKilled again", t0.Add(6*time.Second))
	if err != nil {
		t.Fatalf("ActorCrashed 2: %v", err)
	}
	if op != lifecycle.SubstrateOpSuspendActor {
		t.Errorf("op = %q, want SuspendActor", op)
	}
	if st.GetState() != v1alpha1.TaskState_TASK_STATE_FAILED || st.GetStatusDetail().GetReason() != lifecycle.ReasonSandboxCrashed {
		t.Errorf("state=%v detail=%v, want FAILED/SANDBOX_CRASHED", st.GetState(), st.GetStatusDetail())
	}
}

func TestBudgetExhaustedAndCancel(t *testing.T) {
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	spec := &v1alpha1.TaskSpec{
		Budget: &v1alpha1.Budget{MaxWallclock: durationpb.New(30 * time.Second)},
	}
	st := &v1alpha1.TaskStatus{}
	_, _ = lifecycle.CreateTask(spec, st, t0)
	_, _ = lifecycle.CommandStarted(st, t0)

	if lifecycle.IsBudgetExhausted(spec, st, t0.Add(29*time.Second)) {
		t.Error("expected budget not exhausted at 29s")
	}
	if !lifecycle.IsBudgetExhausted(spec, st, t0.Add(30*time.Second)) {
		t.Error("expected budget exhausted at 30s")
	}

	if _, err := lifecycle.BudgetExhausted(st, "wallclock limit 30s exceeded", t0.Add(30*time.Second)); err != nil {
		t.Fatalf("BudgetExhausted: %v", err)
	}
	if st.GetState() != v1alpha1.TaskState_TASK_STATE_FAILED || st.GetStatusDetail().GetReason() != lifecycle.ReasonBudgetExhausted {
		t.Errorf("after BudgetExhausted: state=%v detail=%v", st.GetState(), st.GetStatusDetail())
	}

	// CancelTask from any non-terminal state transitions to CANCELLED
	for _, initState := range []v1alpha1.TaskState{
		v1alpha1.TaskState_TASK_STATE_QUEUED,
		v1alpha1.TaskState_TASK_STATE_WORKING,
		v1alpha1.TaskState_TASK_STATE_IDLE,
		v1alpha1.TaskState_TASK_STATE_SUSPENDING,
		v1alpha1.TaskState_TASK_STATE_SUSPENDED,
	} {
		s := &v1alpha1.TaskStatus{State: initState, StartTime: st.StartTime}
		if _, err := lifecycle.CancelTask(s, t0.Add(5*time.Second)); err != nil {
			t.Errorf("CancelTask from %s: %v", initState, err)
		}
		if s.GetState() != v1alpha1.TaskState_TASK_STATE_CANCELLED ||
			s.GetOutcome().GetResult() != v1alpha1.TaskResult_TASK_RESULT_CANCELLED ||
			s.GetPhase() != v1alpha1.PhaseCancelled {
			t.Errorf("CancelTask from %s produced state=%v outcome=%v phase=%q", initState, s.GetState(), s.GetOutcome(), s.GetPhase())
		}
	}
}

func TestTerminalStatesRejectFurtherTransitions(t *testing.T) {
	now := time.Now()
	spec := &v1alpha1.TaskSpec{Retry: &v1alpha1.RetryConfig{MaxRetries: 3}}

	for _, term := range []v1alpha1.TaskState{
		v1alpha1.TaskState_TASK_STATE_COMPLETED,
		v1alpha1.TaskState_TASK_STATE_FAILED,
		v1alpha1.TaskState_TASK_STATE_CANCELLED,
	} {
		t.Run(term.String(), func(t *testing.T) {
			checkErr := func(name string, err error) {
				t.Helper()
				if !errors.Is(err, lifecycle.ErrTerminalState) {
					t.Errorf("%s on %s: got %v, want ErrTerminalState", name, term, err)
				}
			}
			_, err := lifecycle.CreateTask(spec, &v1alpha1.TaskStatus{State: term}, now)
			checkErr("CreateTask", err)
			_, err = lifecycle.CommandStarted(&v1alpha1.TaskStatus{State: term}, now)
			checkErr("CommandStarted", err)
			_, err = lifecycle.IdleNotice(&v1alpha1.TaskStatus{State: term}, now)
			checkErr("IdleNotice", err)
			_, err = lifecycle.SuspendTask(&v1alpha1.TaskStatus{State: term}, now)
			checkErr("SuspendTask", err)
			_, err = lifecycle.ResumeTask(&v1alpha1.TaskStatus{State: term}, now)
			checkErr("ResumeTask", err)
			_, err = lifecycle.Eviction(&v1alpha1.TaskStatus{State: term}, now)
			checkErr("Eviction", err)
			_, err = lifecycle.CommandExited(spec, &v1alpha1.TaskStatus{State: term}, 0, "", nil, now)
			checkErr("CommandExited", err)
			_, err = lifecycle.ActorCrashed(spec, &v1alpha1.TaskStatus{State: term}, "crash", now)
			checkErr("ActorCrashed", err)
			_, err = lifecycle.BudgetExhausted(&v1alpha1.TaskStatus{State: term}, "budget", now)
			checkErr("BudgetExhausted", err)
			_, err = lifecycle.CancelTask(&v1alpha1.TaskStatus{State: term}, now)
			checkErr("CancelTask", err)
		})
	}
}
