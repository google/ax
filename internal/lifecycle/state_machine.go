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

package lifecycle

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	// ErrTerminalState is returned when a mutating transition is attempted on a
	// task in a terminal state (COMPLETED, FAILED, CANCELLED).
	ErrTerminalState = errors.New("task is in a terminal state and cannot transition")
	// ErrInvalidTransition is returned when an event is not valid for the task's
	// current state.
	ErrInvalidTransition = errors.New("invalid task state transition")
)

// StatusDetail reasons explaining why a task suspended or failed.
const (
	ReasonAdminSuspended  = "ADMIN_SUSPENDED"
	ReasonIdleTimeout     = "IDLE_TIMEOUT"
	ReasonDeferred        = "DEFERRED"
	ReasonCommandFailed   = "COMMAND_FAILED"
	ReasonSandboxCrashed  = "SANDBOX_CRASHED"
	ReasonBudgetExhausted = "BUDGET_EXHAUSTED"
	ReasonSetupFailed     = "SETUP_FAILED"
)

// SubstrateOp identifies the Substrate actor operation required by a state transition.
type SubstrateOp string

const (
	SubstrateOpNone         SubstrateOp = ""
	SubstrateOpCreateActor  SubstrateOp = "CreateActor"
	SubstrateOpResumeActor  SubstrateOp = "ResumeActor"
	SubstrateOpSuspendActor SubstrateOp = "SuspendActor"
	SubstrateOpRevertActor  SubstrateOp = "RevertActor"
)

// IsTerminal reports whether state is a terminal, immutable TaskState.
func IsTerminal(state v1alpha1.TaskState) bool {
	switch state {
	case v1alpha1.TaskState_TASK_STATE_COMPLETED,
		v1alpha1.TaskState_TASK_STATE_FAILED,
		v1alpha1.TaskState_TASK_STATE_CANCELLED:
		return true
	default:
		return false
	}
}

// IsStickyState reports whether state is owned by AX's state machine even when
// the underlying Substrate actor is parked (SUSPENDED) or inspected via ax ssh
// (RUNNING). Terminal states and IDLE are never overwritten by live actor state.
func IsStickyState(state v1alpha1.TaskState) bool {
	return IsTerminal(state) || state == v1alpha1.TaskState_TASK_STATE_IDLE
}

// PhaseFromState maps a TaskState to the legacy status.phase string kept in
// sync through M1 for CLI and API compatibility.
func PhaseFromState(state v1alpha1.TaskState) string {
	switch state {
	case v1alpha1.TaskState_TASK_STATE_QUEUED:
		return v1alpha1.PhasePending
	case v1alpha1.TaskState_TASK_STATE_WORKING:
		return v1alpha1.PhaseRunning
	case v1alpha1.TaskState_TASK_STATE_SUSPENDING:
		return v1alpha1.PhaseSuspending
	case v1alpha1.TaskState_TASK_STATE_SUSPENDED:
		return v1alpha1.PhaseSuspended
	case v1alpha1.TaskState_TASK_STATE_IDLE:
		return v1alpha1.PhaseIdle
	case v1alpha1.TaskState_TASK_STATE_COMPLETED:
		return v1alpha1.PhaseCompleted
	case v1alpha1.TaskState_TASK_STATE_FAILED:
		return v1alpha1.PhaseFailed
	case v1alpha1.TaskState_TASK_STATE_CANCELLED:
		return v1alpha1.PhaseCancelled
	case v1alpha1.TaskState_TASK_STATE_REQUIRES_ACTION:
		return "RequiresAction"
	default:
		return ""
	}
}

// CreateTask initializes a newly created task's status.
// Root or child tasks start in TASK_STATE_QUEUED with attempt = 1, unless
// spec.GetSuspend() is true, in which case they start in TASK_STATE_SUSPENDED
// with StatusDetail{Reason: "ADMIN_SUSPENDED"}.
func CreateTask(spec *v1alpha1.TaskSpec, st *v1alpha1.TaskStatus, now time.Time) (SubstrateOp, error) {
	if st == nil {
		return SubstrateOpNone, errors.New("nil task status")
	}
	if IsTerminal(st.State) {
		return SubstrateOpNone, fmt.Errorf("%w: %s", ErrTerminalState, st.State)
	}
	st.Attempt = 1
	st.Outcome = nil
	st.EndTime = nil
	if pid := spec.GetParentTaskId(); pid != "" {
		st.ParentTaskId = pid
	}
	if spec.GetSuspend() {
		st.State = v1alpha1.TaskState_TASK_STATE_SUSPENDED
		st.StatusDetail = &v1alpha1.StatusDetail{Reason: ReasonAdminSuspended}
		st.Phase = PhaseFromState(st.State)
		return SubstrateOpSuspendActor, nil
	}
	st.State = v1alpha1.TaskState_TASK_STATE_QUEUED
	st.StatusDetail = nil
	st.Phase = PhaseFromState(st.State)
	return SubstrateOpCreateActor, nil
}

// CommandStarted transitions a task from QUEUED (or warm IDLE wakeup) to WORKING
// and sets start_time for the new working interval.
func CommandStarted(st *v1alpha1.TaskStatus, now time.Time) (SubstrateOp, error) {
	if st == nil {
		return SubstrateOpNone, errors.New("nil task status")
	}
	if IsTerminal(st.State) {
		return SubstrateOpNone, fmt.Errorf("%w: %s", ErrTerminalState, st.State)
	}
	switch st.State {
	case v1alpha1.TaskState_TASK_STATE_QUEUED,
		v1alpha1.TaskState_TASK_STATE_IDLE,
		v1alpha1.TaskState_TASK_STATE_UNSPECIFIED:
		if st.Attempt <= 0 {
			st.Attempt = 1
		}
		st.State = v1alpha1.TaskState_TASK_STATE_WORKING
		st.StatusDetail = nil
		if !now.IsZero() {
			st.StartTime = timestamppb.New(now)
		}
		st.Phase = PhaseFromState(st.State)
		return SubstrateOpNone, nil
	default:
		return SubstrateOpNone, fmt.Errorf("%w: cannot start command from %s", ErrInvalidTransition, st.State)
	}
}

// BeginSuspend transitions a task into TASK_STATE_SUSPENDING prior to snapshotting
// its actor in Substrate, accruing working_duration if the task was WORKING.
func BeginSuspend(st *v1alpha1.TaskStatus, reason, detail string, now time.Time) (SubstrateOp, error) {
	if st == nil {
		return SubstrateOpNone, errors.New("nil task status")
	}
	if IsTerminal(st.State) {
		return SubstrateOpNone, fmt.Errorf("%w: %s", ErrTerminalState, st.State)
	}
	switch reason {
	case ReasonIdleTimeout, ReasonDeferred:
		if st.State != v1alpha1.TaskState_TASK_STATE_WORKING {
			return SubstrateOpNone, fmt.Errorf("%w: %s requires WORKING state, got %s", ErrInvalidTransition, reason, st.State)
		}
	case ReasonAdminSuspended:
		switch st.State {
		case v1alpha1.TaskState_TASK_STATE_QUEUED,
			v1alpha1.TaskState_TASK_STATE_WORKING,
			v1alpha1.TaskState_TASK_STATE_IDLE,
			v1alpha1.TaskState_TASK_STATE_SUSPENDING,
			v1alpha1.TaskState_TASK_STATE_SUSPENDED,
			v1alpha1.TaskState_TASK_STATE_UNSPECIFIED:
		default:
			return SubstrateOpNone, fmt.Errorf("%w: cannot suspend from %s", ErrInvalidTransition, st.State)
		}
	default:
		return SubstrateOpNone, fmt.Errorf("%w: unknown suspend reason %q", ErrInvalidTransition, reason)
	}

	if st.State == v1alpha1.TaskState_TASK_STATE_WORKING {
		accrueWorkingDuration(st, now)
	}
	if st.Attempt <= 0 {
		st.Attempt = 1
	}
	st.State = v1alpha1.TaskState_TASK_STATE_SUSPENDING
	st.StatusDetail = &v1alpha1.StatusDetail{Reason: reason, Detail: detail}
	st.Phase = PhaseFromState(st.State)
	return SubstrateOpSuspendActor, nil
}

// CompleteSuspend finishes a SUSPENDING transition once the actor snapshot completes,
// moving the task to IDLE (turn idle), SUSPENDED (user pause), or QUEUED (worker eviction).
func CompleteSuspend(st *v1alpha1.TaskStatus) error {
	if st == nil {
		return errors.New("nil task status")
	}
	if IsTerminal(st.State) {
		return fmt.Errorf("%w: %s", ErrTerminalState, st.State)
	}
	if st.State != v1alpha1.TaskState_TASK_STATE_SUSPENDING {
		return fmt.Errorf("%w: CompleteSuspend requires SUSPENDING state, got %s", ErrInvalidTransition, st.State)
	}
	switch st.GetStatusDetail().GetReason() {
	case ReasonIdleTimeout:
		st.State = v1alpha1.TaskState_TASK_STATE_IDLE
	case ReasonDeferred:
		st.State = v1alpha1.TaskState_TASK_STATE_QUEUED
	default:
		if st.StatusDetail == nil {
			st.StatusDetail = &v1alpha1.StatusDetail{Reason: ReasonAdminSuspended}
		}
		st.State = v1alpha1.TaskState_TASK_STATE_SUSPENDED
	}
	st.Phase = PhaseFromState(st.State)
	return nil
}

// IdleNotice transitions WORKING -> SUSPENDING -> IDLE with reason IDLE_TIMEOUT
// and accrues working_duration.
func IdleNotice(st *v1alpha1.TaskStatus, now time.Time) (SubstrateOp, error) {
	op, err := BeginSuspend(st, ReasonIdleTimeout, "", now)
	if err != nil {
		return SubstrateOpNone, err
	}
	if err := CompleteSuspend(st); err != nil {
		return SubstrateOpNone, err
	}
	return op, nil
}

// SuspendTask transitions QUEUED/WORKING/IDLE -> SUSPENDING -> SUSPENDED with
// reason ADMIN_SUSPENDED and accrues working_duration if WORKING.
func SuspendTask(st *v1alpha1.TaskStatus, now time.Time) (SubstrateOp, error) {
	op, err := BeginSuspend(st, ReasonAdminSuspended, "", now)
	if err != nil {
		return SubstrateOpNone, err
	}
	if err := CompleteSuspend(st); err != nil {
		return SubstrateOpNone, err
	}
	return op, nil
}

// EnqueueResume transitions SUSPENDED or IDLE to QUEUED prior to resuming the
// actor on Substrate.
func EnqueueResume(st *v1alpha1.TaskStatus) (SubstrateOp, error) {
	if st == nil {
		return SubstrateOpNone, errors.New("nil task status")
	}
	if IsTerminal(st.State) {
		return SubstrateOpNone, fmt.Errorf("%w: %s", ErrTerminalState, st.State)
	}
	switch st.State {
	case v1alpha1.TaskState_TASK_STATE_SUSPENDED,
		v1alpha1.TaskState_TASK_STATE_IDLE,
		v1alpha1.TaskState_TASK_STATE_QUEUED,
		v1alpha1.TaskState_TASK_STATE_UNSPECIFIED:
		if st.Attempt <= 0 {
			st.Attempt = 1
		}
		st.State = v1alpha1.TaskState_TASK_STATE_QUEUED
		st.StatusDetail = nil
		st.Phase = PhaseFromState(st.State)
		return SubstrateOpResumeActor, nil
	case v1alpha1.TaskState_TASK_STATE_WORKING:
		return SubstrateOpResumeActor, nil
	default:
		return SubstrateOpNone, fmt.Errorf("%w: cannot resume from %s", ErrInvalidTransition, st.State)
	}
}

// ResumeTask transitions SUSPENDED/IDLE -> QUEUED -> WORKING and records start_time.
func ResumeTask(st *v1alpha1.TaskStatus, now time.Time) (SubstrateOp, error) {
	op, err := EnqueueResume(st)
	if err != nil {
		return SubstrateOpNone, err
	}
	if st.State == v1alpha1.TaskState_TASK_STATE_WORKING {
		return op, nil
	}
	if _, err := CommandStarted(st, now); err != nil {
		return SubstrateOpNone, err
	}
	return op, nil
}

// Eviction transitions WORKING -> SUSPENDING (DEFERRED) -> QUEUED and accrues
// working_duration.
func Eviction(st *v1alpha1.TaskStatus, now time.Time) (SubstrateOp, error) {
	op, err := BeginSuspend(st, ReasonDeferred, "", now)
	if err != nil {
		return SubstrateOpNone, err
	}
	if err := CompleteSuspend(st); err != nil {
		return SubstrateOpNone, err
	}
	return op, nil
}

// CommandExited evaluates command completion from WORKING:
// - Exit 0: transitions WORKING -> COMPLETED with SUCCEEDED outcome.
// - Exit != 0: if attempt <= spec.retry.max_retries, increments attempt++ and
//   stays WORKING (in-place retry); otherwise transitions to FAILED (COMMAND_FAILED).
func CommandExited(spec *v1alpha1.TaskSpec, st *v1alpha1.TaskStatus, exitCode int32, output string, structuredOutput *structpb.Struct, now time.Time) (SubstrateOp, error) {
	if st == nil {
		return SubstrateOpNone, errors.New("nil task status")
	}
	if IsTerminal(st.State) {
		return SubstrateOpNone, fmt.Errorf("%w: %s", ErrTerminalState, st.State)
	}
	if st.State != v1alpha1.TaskState_TASK_STATE_WORKING {
		return SubstrateOpNone, fmt.Errorf("%w: CommandExited requires WORKING state, got %s", ErrInvalidTransition, st.State)
	}
	if st.Attempt <= 0 {
		st.Attempt = 1
	}
	accrueWorkingDuration(st, now)

	if exitCode == 0 {
		st.State = v1alpha1.TaskState_TASK_STATE_COMPLETED
		st.StatusDetail = nil
		st.Outcome = &v1alpha1.TaskOutcome{
			Result:           v1alpha1.TaskResult_TASK_RESULT_SUCCEEDED,
			ExitCode:         0,
			Output:           output,
			StructuredOutput: structuredOutput,
		}
		if !now.IsZero() {
			st.EndTime = timestamppb.New(now)
		}
		st.Phase = PhaseFromState(st.State)
		return SubstrateOpSuspendActor, nil
	}

	maxRetries := spec.GetRetry().GetMaxRetries()
	if st.Attempt <= maxRetries {
		st.Attempt++
		st.State = v1alpha1.TaskState_TASK_STATE_WORKING
		st.StatusDetail = &v1alpha1.StatusDetail{Reason: ReasonCommandFailed}
		if !now.IsZero() {
			st.StartTime = timestamppb.New(now)
		}
		st.Phase = PhaseFromState(st.State)
		return SubstrateOpNone, nil
	}

	st.State = v1alpha1.TaskState_TASK_STATE_FAILED
	st.StatusDetail = &v1alpha1.StatusDetail{Reason: ReasonCommandFailed}
	st.Outcome = &v1alpha1.TaskOutcome{
		Result:           v1alpha1.TaskResult_TASK_RESULT_FAILED,
		ExitCode:         exitCode,
		Output:           output,
		StructuredOutput: structuredOutput,
	}
	if !now.IsZero() {
		st.EndTime = timestamppb.New(now)
	}
	st.Phase = PhaseFromState(st.State)
	return SubstrateOpSuspendActor, nil
}

// ActorCrashed evaluates a sandbox crash:
// - If attempt <= spec.retry.max_retries, increments attempt++ and transitions
//   to QUEUED (SANDBOX_CRASHED) with SubstrateOpRevertActor.
// - Otherwise transitions to FAILED (SANDBOX_CRASHED).
func ActorCrashed(spec *v1alpha1.TaskSpec, st *v1alpha1.TaskStatus, detail string, now time.Time) (SubstrateOp, error) {
	if st == nil {
		return SubstrateOpNone, errors.New("nil task status")
	}
	if IsTerminal(st.State) {
		return SubstrateOpNone, fmt.Errorf("%w: %s", ErrTerminalState, st.State)
	}
	switch st.State {
	case v1alpha1.TaskState_TASK_STATE_WORKING,
		v1alpha1.TaskState_TASK_STATE_QUEUED,
		v1alpha1.TaskState_TASK_STATE_SUSPENDING:
	default:
		return SubstrateOpNone, fmt.Errorf("%w: ActorCrashed not valid from %s", ErrInvalidTransition, st.State)
	}
	if st.State == v1alpha1.TaskState_TASK_STATE_WORKING {
		accrueWorkingDuration(st, now)
	}
	if st.Attempt <= 0 {
		st.Attempt = 1
	}

	maxRetries := spec.GetRetry().GetMaxRetries()
	if st.Attempt <= maxRetries {
		st.Attempt++
		st.State = v1alpha1.TaskState_TASK_STATE_QUEUED
		st.StatusDetail = &v1alpha1.StatusDetail{Reason: ReasonSandboxCrashed, Detail: detail}
		st.Phase = PhaseFromState(st.State)
		return SubstrateOpRevertActor, nil
	}

	st.State = v1alpha1.TaskState_TASK_STATE_FAILED
	st.StatusDetail = &v1alpha1.StatusDetail{Reason: ReasonSandboxCrashed, Detail: detail}
	st.Outcome = &v1alpha1.TaskOutcome{
		Result: v1alpha1.TaskResult_TASK_RESULT_FAILED,
	}
	if !now.IsZero() {
		st.EndTime = timestamppb.New(now)
	}
	st.Phase = PhaseFromState(st.State)
	return SubstrateOpSuspendActor, nil
}

// BudgetExhausted transitions WORKING -> FAILED with reason BUDGET_EXHAUSTED.
func BudgetExhausted(st *v1alpha1.TaskStatus, detail string, now time.Time) (SubstrateOp, error) {
	if st == nil {
		return SubstrateOpNone, errors.New("nil task status")
	}
	if IsTerminal(st.State) {
		return SubstrateOpNone, fmt.Errorf("%w: %s", ErrTerminalState, st.State)
	}
	if st.State != v1alpha1.TaskState_TASK_STATE_WORKING {
		return SubstrateOpNone, fmt.Errorf("%w: BudgetExhausted requires WORKING state, got %s", ErrInvalidTransition, st.State)
	}
	accrueWorkingDuration(st, now)
	st.State = v1alpha1.TaskState_TASK_STATE_FAILED
	st.StatusDetail = &v1alpha1.StatusDetail{Reason: ReasonBudgetExhausted, Detail: detail}
	st.Outcome = &v1alpha1.TaskOutcome{
		Result: v1alpha1.TaskResult_TASK_RESULT_FAILED,
	}
	if !now.IsZero() {
		st.EndTime = timestamppb.New(now)
	}
	st.Phase = PhaseFromState(st.State)
	return SubstrateOpSuspendActor, nil
}

// SetupFailed transitions QUEUED -> FAILED with reason SETUP_FAILED.
func SetupFailed(st *v1alpha1.TaskStatus, detail string, now time.Time) (SubstrateOp, error) {
	if st == nil {
		return SubstrateOpNone, errors.New("nil task status")
	}
	if IsTerminal(st.State) {
		return SubstrateOpNone, fmt.Errorf("%w: %s", ErrTerminalState, st.State)
	}
	if st.State != v1alpha1.TaskState_TASK_STATE_QUEUED && st.State != v1alpha1.TaskState_TASK_STATE_UNSPECIFIED {
		return SubstrateOpNone, fmt.Errorf("%w: SetupFailed requires QUEUED state, got %s", ErrInvalidTransition, st.State)
	}
	st.State = v1alpha1.TaskState_TASK_STATE_FAILED
	st.StatusDetail = &v1alpha1.StatusDetail{Reason: ReasonSetupFailed, Detail: detail}
	st.Outcome = &v1alpha1.TaskOutcome{
		Result: v1alpha1.TaskResult_TASK_RESULT_FAILED,
	}
	if !now.IsZero() {
		st.EndTime = timestamppb.New(now)
	}
	st.Phase = PhaseFromState(st.State)
	return SubstrateOpSuspendActor, nil
}

// CancelTask transitions any non-terminal state to CANCELLED with TASK_RESULT_CANCELLED.
// Terminal states (COMPLETED, FAILED, CANCELLED) return ErrTerminalState.
func CancelTask(st *v1alpha1.TaskStatus, now time.Time) (SubstrateOp, error) {
	if st == nil {
		return SubstrateOpNone, errors.New("nil task status")
	}
	if IsTerminal(st.State) {
		return SubstrateOpNone, fmt.Errorf("%w: %s", ErrTerminalState, st.State)
	}
	if st.State == v1alpha1.TaskState_TASK_STATE_WORKING {
		accrueWorkingDuration(st, now)
	}
	if st.Attempt <= 0 {
		st.Attempt = 1
	}
	st.State = v1alpha1.TaskState_TASK_STATE_CANCELLED
	st.Outcome = &v1alpha1.TaskOutcome{
		Result: v1alpha1.TaskResult_TASK_RESULT_CANCELLED,
	}
	if !now.IsZero() {
		st.EndTime = timestamppb.New(now)
	}
	st.Phase = PhaseFromState(st.State)
	return SubstrateOpSuspendActor, nil
}

// IsBudgetExhausted reports whether the task's cumulative working_duration
// (including any active WORKING interval up to now) has reached spec.budget.max_wallclock.
func IsBudgetExhausted(spec *v1alpha1.TaskSpec, st *v1alpha1.TaskStatus, now time.Time) bool {
	maxWallclock := spec.GetBudget().GetMaxWallclock()
	if maxWallclock == nil || maxWallclock.AsDuration() <= 0 {
		return false
	}
	total := st.GetWorkingDuration().AsDuration()
	if st.GetState() == v1alpha1.TaskState_TASK_STATE_WORKING && st.GetStartTime() != nil && !now.IsZero() {
		if elapsed := now.Sub(st.GetStartTime().AsTime()); elapsed > 0 {
			total += elapsed
		}
	}
	return total >= maxWallclock.AsDuration()
}

// NextRetryBackoff computes the exponential backoff before the given retry attempt
// (where attempt >= 2 is the first retry).
func NextRetryBackoff(retry *v1alpha1.RetryConfig, attempt int32) time.Duration {
	if retry == nil || retry.GetInitialBackoff() == nil {
		return 0
	}
	initial := retry.GetInitialBackoff().AsDuration()
	if initial <= 0 {
		return 0
	}
	mult := retry.GetBackoffMultiplier()
	if mult <= 0 {
		mult = 2.0
	}
	retryIndex := float64(attempt - 2)
	if retryIndex < 0 {
		retryIndex = 0
	}
	d := time.Duration(float64(initial) * math.Pow(mult, retryIndex))
	if maxB := retry.GetMaxBackoff().AsDuration(); maxB > 0 && d > maxB {
		d = maxB
	}
	return d
}

func accrueWorkingDuration(st *v1alpha1.TaskStatus, now time.Time) {
	if st.StartTime == nil || now.IsZero() {
		return
	}
	elapsed := now.Sub(st.StartTime.AsTime())
	if elapsed <= 0 {
		return
	}
	prev := st.GetWorkingDuration().AsDuration()
	st.WorkingDuration = durationpb.New(prev + elapsed)
}
