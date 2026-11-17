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

// Package lifecycle implements pure, side-effect-free state transitions for
// pull-based task assignment claims and runner event reporting.
//
// All functions in this package operate on in-memory [*store.TaskRow] values
// without performing network or database I/O, making them safe to execute
// directly inside a [store.TaskApplier.Apply] callback.
package lifecycle

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/ax/internal/store"
	runnerv1 "github.com/google/ax/pkg/apis/runner/v1"
	"github.com/google/ax/pkg/apis/v1alpha1"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	runnerTokenBytes          = 32
	unexpectedRestartErrorMsg = "container restarted before command exited"
)

// ErrUnauthorized is returned when a runner request omits or presents a bearer
// token that does not match the active actor's stored token digest.
var ErrUnauthorized = errors.New("invalid or missing runner token")

// TokenMinter generates a cryptographically random bearer token along with its
// SHA-256 digest for storage in [store.TaskRow.RunnerTokenHash].
type TokenMinter func() (raw string, hash []byte, err error)

// DefaultMintToken mints a 256-bit hex-encoded bearer token and returns both
// the raw token string and its SHA-256 digest.
func DefaultMintToken() (string, []byte, error) {
	var buf [runnerTokenBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", nil, fmt.Errorf("generating runner token: %w", err)
	}
	raw := hex.EncodeToString(buf[:])
	return raw, HashToken(raw), nil
}

// HashToken returns the SHA-256 digest of raw, or nil when raw is empty.
func HashToken(raw string) []byte {
	if raw == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

// VerifyToken reports in constant time whether raw hashes to expectedHash.
func VerifyToken(raw string, expectedHash []byte) bool {
	if raw == "" || len(expectedHash) != sha256.Size {
		return false
	}
	sum := sha256.Sum256([]byte(raw))
	return subtle.ConstantTimeCompare(sum[:], expectedHash) == 1
}

// IsTerminal reports whether state is a terminal task lifecycle state
// (COMPLETED, FAILED, or CANCELLED).
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

// IsExecutionTerminal reports whether task has reached a terminal execution
// outcome that forbids subsequent SuspendTask or ResumeTask operations.
//
// Tasks recorded as FAILED with reason PROVISIONING_FAILED and no execution
// Outcome failed during control-plane reconciliation (for example, before a
// bound Gateway existed) and remain eligible for retry via ResumeTask or
// SuspendTask once the underlying precondition is resolved.
func IsExecutionTerminal(task *v1alpha1.Task) bool {
	st := task.GetStatus()
	switch st.GetState() {
	case v1alpha1.TaskState_TASK_STATE_COMPLETED,
		v1alpha1.TaskState_TASK_STATE_CANCELLED:
		return true
	case v1alpha1.TaskState_TASK_STATE_FAILED:
		if st.GetOutcome() != nil {
			return true
		}
		return st.GetStatusDetail().GetReason() != v1alpha1.TaskStatusReason_TASK_STATUS_REASON_PROVISIONING_FAILED
	default:
		return false
	}
}

// Claim evaluates an assignment claim from ax-task-runner against row and
// returns the updated row (or row itself when no stored fields changed) along
// with the ClaimAssignmentResponse to send back to the runner.
//
// On the first claim (or a lost-response retry with empty presentedToken and
// matching non-empty ClaimNonce), Claim mints a fresh bearer token. Once the
// runner presents a valid X-AX-Runner-Token, RunnerTokenConfirmed is set and
// any missing or mismatched token thereafter fails with [ErrUnauthorized].
func Claim(row *store.TaskRow, req *runnerv1.ClaimAssignmentRequest, ws *v1alpha1.Workspace, presentedToken string, mintToken TokenMinter, now time.Time) (*store.TaskRow, *runnerv1.ClaimAssignmentResponse, error) {
	if row == nil || row.Task == nil {
		return nil, nil, errors.New("lifecycle: nil task row")
	}
	if req == nil {
		req = &runnerv1.ClaimAssignmentRequest{}
	}
	if mintToken == nil {
		mintToken = DefaultMintToken
	}

	next := row.Clone()
	mutated := false
	var mintedToken string

	switch {
	case len(next.RunnerTokenHash) == 0:
		raw, hash, err := mintToken()
		if err != nil {
			return nil, nil, err
		}
		next.RunnerTokenHash = hash
		next.RunnerTokenConfirmed = false
		next.ClaimNonce = req.GetNonce()
		mintedToken = raw
		mutated = true

	case presentedToken != "":
		if !VerifyToken(presentedToken, next.RunnerTokenHash) {
			return nil, nil, ErrUnauthorized
		}
		if !next.RunnerTokenConfirmed || next.ClaimNonce != "" {
			next.RunnerTokenConfirmed = true
			next.ClaimNonce = ""
			mutated = true
		}

	default:
		if next.RunnerTokenConfirmed || req.GetNonce() == "" || req.GetNonce() != next.ClaimNonce {
			return nil, nil, ErrUnauthorized
		}
		raw, hash, err := mintToken()
		if err != nil {
			return nil, nil, err
		}
		next.RunnerTokenHash = hash
		next.RunnerTokenConfirmed = false
		next.ClaimNonce = req.GetNonce()
		mintedToken = raw
		mutated = true
	}

	resultRow := row
	if mutated {
		resultRow = next
	}

	isDeleting := next.Task.GetMetadata().GetDeleteTime() != nil
	if !next.Runnable || isDeleting || IsTerminal(next.Task.GetStatus().GetState()) {
		return resultRow, &runnerv1.ClaimAssignmentResponse{
			Mode:        runnerv1.AssignmentMode_ASSIGNMENT_MODE_IDLE,
			RunnerToken: mintedToken,
		}, nil
	}

	if next.ActiveRunID != "" && req.GetInMemoryRunId() == next.ActiveRunID && req.GetChildAlive() {
		return resultRow, &runnerv1.ClaimAssignmentResponse{
			Mode:        runnerv1.AssignmentMode_ASSIGNMENT_MODE_RESUME,
			RunId:       next.ActiveRunID,
			Attempt:     effectiveAttempt(next.Task),
			RunnerToken: mintedToken,
		}, nil
	}

	restartedSameRun := req.GetLastStartedRunId() == next.ActiveRunID || req.GetInMemoryRunId() == next.ActiveRunID
	if next.ActiveRunID != "" && restartedSameRun && !req.GetChildAlive() {
		if next.Task.Status == nil {
			next.Task.Status = &v1alpha1.TaskStatus{}
		}
		next.Task.Status.State = v1alpha1.TaskState_TASK_STATE_FAILED
		next.Task.Status.StatusDetail = &v1alpha1.TaskStatusDetail{
			Reason: v1alpha1.TaskStatusReason_TASK_STATUS_REASON_SANDBOX_CRASHED,
			Detail: unexpectedRestartErrorMsg,
		}
		setReadyCondition(next.Task, "False", v1alpha1.ReasonFailed, unexpectedRestartErrorMsg, now)
		next.Runnable = false
		next.ActiveRunID = ""
		return next, &runnerv1.ClaimAssignmentResponse{
			Mode:        runnerv1.AssignmentMode_ASSIGNMENT_MODE_IDLE,
			RunnerToken: mintedToken,
		}, nil
	}

	if next.ActiveRunID == "" {
		next.ActiveRunID = next.NextRunID()
		next.LastAckedRunnerSeq = 0
		mutated = true
	}
	if mutated {
		resultRow = next
	}

	var clonedWS *v1alpha1.Workspace
	if ws != nil {
		clonedWS = proto.Clone(ws).(*v1alpha1.Workspace)
	}
	clonedTask := proto.Clone(next.Task).(*v1alpha1.Task)

	return resultRow, &runnerv1.ClaimAssignmentResponse{
		Mode:        runnerv1.AssignmentMode_ASSIGNMENT_MODE_RUN,
		RunId:       next.ActiveRunID,
		Attempt:     effectiveAttempt(next.Task),
		RunnerToken: mintedToken,
		Task:        clonedTask,
		Workspace:   clonedWS,
		Exec:        buildExecSpec(next.Task),
		Goal: &runnerv1.GoalPayload{
			Text: next.Task.GetSpec().GetGoal().GetText(),
		},
	}, nil
}

// ApplyEvents validates the runner's bearer token and applies sequenced
// lifecycle events for the active run.
//
// Events with seq <= LastAckedRunnerSeq are ignored as duplicates. Events for a
// stale or already-completed run_id leave the stored row untouched (beyond
// confirming an unconfirmed token) while acknowledging the batch.
//
// When CommandExited is processed for ActiveRunID, ApplyEvents records the
// terminal COMPLETED or FAILED TaskOutcome, clears ActiveRunID and Runnable,
// returns ASSIGNMENT_MODE_IDLE as the follow-up assignment, and sets parkActor
// true so the caller suspends the finished sandbox asynchronously after
// flushing the HTTP 200 response.
func ApplyEvents(row *store.TaskRow, req *runnerv1.ReportEventsRequest, presentedToken string, now time.Time) (*store.TaskRow, *runnerv1.ReportEventsResponse, bool, error) {
	if row == nil || row.Task == nil {
		return nil, nil, false, errors.New("lifecycle: nil task row")
	}
	if !VerifyToken(presentedToken, row.RunnerTokenHash) {
		return nil, nil, false, ErrUnauthorized
	}

	next := row.Clone()
	mutated := false
	if !next.RunnerTokenConfirmed || next.ClaimNonce != "" {
		next.RunnerTokenConfirmed = true
		next.ClaimNonce = ""
		mutated = true
	}

	if req == nil || len(req.GetEvents()) == 0 {
		if mutated {
			return next, &runnerv1.ReportEventsResponse{AckSeq: next.LastAckedRunnerSeq}, false, nil
		}
		return row, &runnerv1.ReportEventsResponse{AckSeq: row.LastAckedRunnerSeq}, false, nil
	}

	// Ignore events belonging to an old or already-finished run_id so retries
	// after completion or suspend acknowledge cleanly without mutating state.
	if next.ActiveRunID == "" || req.GetRunId() != next.ActiveRunID {
		var maxSeq int64
		for _, ev := range req.GetEvents() {
			if ev.GetSeq() > maxSeq {
				maxSeq = ev.GetSeq()
			}
		}
		resp := &runnerv1.ReportEventsResponse{
			AckSeq: maxSeq,
			Next: &runnerv1.ClaimAssignmentResponse{
				Mode: runnerv1.AssignmentMode_ASSIGNMENT_MODE_IDLE,
			},
		}
		if mutated {
			return next, resp, false, nil
		}
		return row, resp, false, nil
	}

	var (
		nextAssignment *runnerv1.ClaimAssignmentResponse
		parkActor      bool
	)
	for _, ev := range req.GetEvents() {
		if ev == nil || ev.GetSeq() <= next.LastAckedRunnerSeq {
			continue
		}
		next.LastAckedRunnerSeq = ev.GetSeq()
		mutated = true

		if exited := ev.GetCommandExited(); exited != nil {
			applyCommandExited(next, exited, now)
			parkActor = true
			nextAssignment = &runnerv1.ClaimAssignmentResponse{
				Mode: runnerv1.AssignmentMode_ASSIGNMENT_MODE_IDLE,
			}
			break
		}
	}

	resp := &runnerv1.ReportEventsResponse{
		AckSeq: next.LastAckedRunnerSeq,
		Next:   nextAssignment,
	}
	if mutated {
		return next, resp, parkActor, nil
	}
	return row, resp, parkActor, nil
}

func applyCommandExited(row *store.TaskRow, exited *runnerv1.CommandExited, now time.Time) {
	if row.Task.Status == nil {
		row.Task.Status = &v1alpha1.TaskStatus{}
	}
	row.ActiveRunID = ""
	row.Runnable = false
	row.Task.Status.StatusDetail = nil

	exitCode := exited.GetExitCode()
	if exitCode == 0 {
		row.Task.Status.State = v1alpha1.TaskState_TASK_STATE_COMPLETED
		row.Task.Status.Outcome = &v1alpha1.TaskOutcome{
			Result:   v1alpha1.TaskResult_TASK_RESULT_SUCCEEDED,
			ExitCode: 0,
			Output:   exited.GetOutput(),
		}
		setReadyCondition(row.Task, "False", "Completed", "task command completed", now)
		return
	}

	msg := fmt.Sprintf("command exited with code %d", exitCode)
	row.Task.Status.State = v1alpha1.TaskState_TASK_STATE_FAILED
	row.Task.Status.Outcome = &v1alpha1.TaskOutcome{
		Result:   v1alpha1.TaskResult_TASK_RESULT_FAILED,
		ExitCode: exitCode,
		Output:   exited.GetOutput(),
		Status: &rpcstatus.Status{
			Code:    int32(codes.Internal),
			Message: msg,
		},
	}
	setReadyCondition(row.Task, "False", v1alpha1.ReasonFailed, msg, now)
}

func effectiveAttempt(task *v1alpha1.Task) int32 {
	if att := task.GetStatus().GetAttempt(); att > 0 {
		return att
	}
	return 1
}

func buildExecSpec(task *v1alpha1.Task) *runnerv1.ExecSpec {
	spec := task.GetSpec()
	argv := slices.Clone(task.GetStatus().GetResolvedArgv())
	if len(argv) == 0 && len(spec.GetCommands()) > 0 {
		argv = slices.Clone(spec.GetCommands())
	}
	var env map[string]string
	if len(spec.GetEnv()) > 0 {
		env = make(map[string]string, len(spec.GetEnv()))
		for _, e := range spec.GetEnv() {
			if e.GetName() != "" {
				env[e.GetName()] = e.GetValue()
			}
		}
	}
	return &runnerv1.ExecSpec{
		Argv:    argv,
		Workdir: spec.WorkspacePath(),
		Env:     env,
	}
}

func setReadyCondition(task *v1alpha1.Task, condStatus, reason, message string, now time.Time) {
	if task.Status == nil {
		task.Status = &v1alpha1.TaskStatus{}
	}
	ts := timestamppb.New(now)
	for _, c := range task.Status.Conditions {
		if c.GetType() != v1alpha1.ConditionReady {
			continue
		}
		if c.Status != condStatus || c.Reason != reason || c.Message != message {
			c.Status = condStatus
			c.Reason = reason
			c.Message = message
			c.UpdateTime = ts
		}
		return
	}
	task.Status.Conditions = append(task.Status.Conditions, &v1alpha1.Condition{
		Type:       v1alpha1.ConditionReady,
		Status:     condStatus,
		Reason:     reason,
		Message:    message,
		UpdateTime: ts,
	})
}
