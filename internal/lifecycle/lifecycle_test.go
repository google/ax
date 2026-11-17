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
	"testing"
	"time"

	"github.com/google/ax/internal/store"
	runnerv1 "github.com/google/ax/pkg/apis/runner/v1"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

func fixedTokenMinter(tok string) TokenMinter {
	return func() (string, []byte, error) {
		return tok, HashToken(tok), nil
	}
}

func testTaskRow(runnable bool) *store.TaskRow {
	return &store.TaskRow{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{
				Atespace: "default",
				Name:     "demo",
			},
			Spec: &v1alpha1.TaskSpec{
				Commands: []string{"echo", "hi"},
				Goal:     &v1alpha1.Goal{Text: "solve it"},
				Env:      []*v1alpha1.EnvVar{{Name: "FOO", Value: "bar"}},
			},
			Status: &v1alpha1.TaskStatus{
				State:   v1alpha1.TaskState_TASK_STATE_QUEUED,
				Attempt: 1,
			},
		},
		Runnable: runnable,
	}
}

func TestClaimTokenLifecycleAndAssignments(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	row := testTaskRow(false)

	// 1. Initial claim mints token and returns IDLE while not runnable.
	next, resp, err := Claim(row, &runnerv1.ClaimAssignmentRequest{Nonce: "n-1"}, nil, "", fixedTokenMinter("tok-1"), now)
	if err != nil {
		t.Fatalf("initial Claim() error = %v", err)
	}
	if resp.GetMode() != runnerv1.AssignmentMode_ASSIGNMENT_MODE_IDLE || resp.GetRunnerToken() != "tok-1" {
		t.Fatalf("initial Claim() got mode=%v token=%q, want IDLE and tok-1", resp.GetMode(), resp.GetRunnerToken())
	}

	// 2. Lost-response retry with matching nonce and empty token re-mints idempotently.
	next, resp, err = Claim(next, &runnerv1.ClaimAssignmentRequest{Nonce: "n-1"}, nil, "", fixedTokenMinter("tok-2"), now)
	if err != nil {
		t.Fatalf("nonce-retry Claim() error = %v", err)
	}
	if resp.GetRunnerToken() != "tok-2" {
		t.Fatalf("nonce-retry Claim() token = %q, want tok-2", resp.GetRunnerToken())
	}

	// 3. Mismatched nonce without token fails with ErrUnauthorized.
	if _, _, err := Claim(next, &runnerv1.ClaimAssignmentRequest{Nonce: "wrong"}, nil, "", fixedTokenMinter("tok-3"), now); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong-nonce Claim() error = %v, want ErrUnauthorized", err)
	}

	// 4. Presenting valid token confirms it and clears ClaimNonce.
	next.Runnable = true
	ws := &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "ws"}}
	next, resp, err = Claim(next, &runnerv1.ClaimAssignmentRequest{Nonce: "n-2"}, ws, "tok-2", fixedTokenMinter("tok-3"), now)
	if err != nil {
		t.Fatalf("confirmed RUN Claim() error = %v", err)
	}
	if !next.RunnerTokenConfirmed || next.ClaimNonce != "" {
		t.Fatalf("expected RunnerTokenConfirmed=true and empty ClaimNonce, got confirmed=%v nonce=%q", next.RunnerTokenConfirmed, next.ClaimNonce)
	}
	if resp.GetMode() != runnerv1.AssignmentMode_ASSIGNMENT_MODE_RUN || resp.GetRunId() != "r-1" {
		t.Fatalf("RUN Claim() got mode=%v runID=%q, want RUN r-1", resp.GetMode(), resp.GetRunId())
	}
	if resp.GetGoal().GetText() != "solve it" || len(resp.GetExec().GetArgv()) != 2 || resp.GetExec().GetEnv()["FOO"] != "bar" {
		t.Fatalf("RUN Claim() payload mismatch: goal=%q exec=%+v", resp.GetGoal().GetText(), resp.GetExec())
	}
	if resp.GetWorkspace().GetMetadata().GetName() != "ws" {
		t.Fatalf("RUN Claim() missing workspace")
	}

	// 5. Missing token after confirmation fails even with matching nonce.
	if _, _, err := Claim(next, &runnerv1.ClaimAssignmentRequest{Nonce: "n-2"}, nil, "", fixedTokenMinter("tok-4"), now); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("post-confirm empty-token Claim() error = %v, want ErrUnauthorized", err)
	}

	// 6. Live MEMORY restore returns RESUME.
	_, resumeResp, err := Claim(next, &runnerv1.ClaimAssignmentRequest{
		InMemoryRunId: "r-1",
		ChildAlive:    true,
	}, nil, "tok-2", nil, now)
	if err != nil {
		t.Fatalf("RESUME Claim() error = %v", err)
	}
	if resumeResp.GetMode() != runnerv1.AssignmentMode_ASSIGNMENT_MODE_RESUME || resumeResp.GetRunId() != "r-1" {
		t.Fatalf("RESUME Claim() got mode=%v runID=%q, want RESUME r-1", resumeResp.GetMode(), resumeResp.GetRunId())
	}

	// 7. Unexpected container restart mid-run marks FAILED (SANDBOX_CRASHED).
	crashed, crashResp, err := Claim(next, &runnerv1.ClaimAssignmentRequest{
		LastStartedRunId: "r-1",
		ChildAlive:       false,
	}, nil, "tok-2", nil, now)
	if err != nil {
		t.Fatalf("crash-detect Claim() error = %v", err)
	}
	if crashResp.GetMode() != runnerv1.AssignmentMode_ASSIGNMENT_MODE_IDLE {
		t.Fatalf("crash-detect Claim() mode = %v, want IDLE", crashResp.GetMode())
	}
	if crashed.Runnable || crashed.ActiveRunID != "" {
		t.Fatalf("expected Runnable=false and ActiveRunID empty after crash, got runnable=%v runID=%q", crashed.Runnable, crashed.ActiveRunID)
	}
	if crashed.Task.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_FAILED ||
		crashed.Task.GetStatus().GetStatusDetail().GetReason() != v1alpha1.TaskStatusReason_TASK_STATUS_REASON_SANDBOX_CRASHED {
		t.Fatalf("unexpected crash status: %+v", crashed.Task.GetStatus())
	}
}

func TestApplyEventsCompletionAndFencing(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	row := testTaskRow(true)
	row.RunnerTokenHash = HashToken("tok")
	row.ActiveRunID = "r-1"

	// Unauthorized token fails.
	if _, _, _, err := ApplyEvents(row, &runnerv1.ReportEventsRequest{RunId: "r-1"}, "bad", now); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("ApplyEvents(bad token) error = %v, want ErrUnauthorized", err)
	}

	// CommandStarted advances AckSeq without parking.
	next, resp, park, err := ApplyEvents(row, &runnerv1.ReportEventsRequest{
		RunId: "r-1",
		Events: []*runnerv1.RunnerEvent{{
			Seq:   1,
			Event: &runnerv1.RunnerEvent_CommandStarted{CommandStarted: &runnerv1.CommandStarted{Pid: 42}},
		}},
	}, "tok", now)
	if err != nil {
		t.Fatalf("ApplyEvents(CommandStarted) error = %v", err)
	}
	if park || resp.GetAckSeq() != 1 || next.LastAckedRunnerSeq != 1 || !next.RunnerTokenConfirmed {
		t.Fatalf("unexpected CommandStarted result: park=%v ack=%d seq=%d confirmed=%v", park, resp.GetAckSeq(), next.LastAckedRunnerSeq, next.RunnerTokenConfirmed)
	}

	// Duplicate seq <= LastAckedRunnerSeq is ignored.
	_, dupResp, dupPark, err := ApplyEvents(next, &runnerv1.ReportEventsRequest{
		RunId: "r-1",
		Events: []*runnerv1.RunnerEvent{{
			Seq:   1,
			Event: &runnerv1.RunnerEvent_CommandExited{CommandExited: &runnerv1.CommandExited{ExitCode: 0}},
		}},
	}, "tok", now)
	if err != nil {
		t.Fatalf("ApplyEvents(dup) error = %v", err)
	}
	if dupPark || dupResp.GetAckSeq() != 1 {
		t.Fatalf("expected duplicate event to be ignored, got park=%v ack=%d", dupPark, dupResp.GetAckSeq())
	}

	// Non-zero CommandExited marks FAILED and requests actor parking.
	failed, failResp, park, err := ApplyEvents(next, &runnerv1.ReportEventsRequest{
		RunId: "r-1",
		Events: []*runnerv1.RunnerEvent{{
			Seq: 2,
			Event: &runnerv1.RunnerEvent_CommandExited{CommandExited: &runnerv1.CommandExited{
				ExitCode: 2,
				Output:   "boom",
			}},
		}},
	}, "tok", now)
	if err != nil {
		t.Fatalf("ApplyEvents(CommandExited) error = %v", err)
	}
	if !park || failResp.GetAckSeq() != 2 || failResp.GetNext().GetMode() != runnerv1.AssignmentMode_ASSIGNMENT_MODE_IDLE {
		t.Fatalf("unexpected CommandExited response: park=%v resp=%+v", park, failResp)
	}
	if failed.Runnable || failed.ActiveRunID != "" {
		t.Fatalf("expected Runnable=false and ActiveRunID empty, got runnable=%v runID=%q", failed.Runnable, failed.ActiveRunID)
	}
	if failed.Task.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_FAILED ||
		failed.Task.GetStatus().GetOutcome().GetExitCode() != 2 ||
		failed.Task.GetStatus().GetOutcome().GetOutput() != "boom" {
		t.Fatalf("unexpected failed task status: %+v", failed.Task.GetStatus())
	}

	// Stale run_id retry acknowledges without mutating or parking again.
	_, staleResp, stalePark, err := ApplyEvents(failed, &runnerv1.ReportEventsRequest{
		RunId: "r-1",
		Events: []*runnerv1.RunnerEvent{{
			Seq:   2,
			Event: &runnerv1.RunnerEvent_CommandExited{CommandExited: &runnerv1.CommandExited{ExitCode: 0}},
		}},
	}, "tok", now)
	if err != nil {
		t.Fatalf("ApplyEvents(stale) error = %v", err)
	}
	if stalePark || staleResp.GetAckSeq() != 2 || staleResp.GetNext().GetMode() != runnerv1.AssignmentMode_ASSIGNMENT_MODE_IDLE {
		t.Fatalf("unexpected stale response: park=%v resp=%+v", stalePark, staleResp)
	}
}

func TestIsExecutionTerminal(t *testing.T) {
	cases := []struct {
		name string
		task *v1alpha1.Task
		want bool
	}{
		{
			name: "queued",
			task: &v1alpha1.Task{Status: &v1alpha1.TaskStatus{State: v1alpha1.TaskState_TASK_STATE_QUEUED}},
			want: false,
		},
		{
			name: "provisioning_failed_retryable",
			task: &v1alpha1.Task{Status: &v1alpha1.TaskStatus{
				State:        v1alpha1.TaskState_TASK_STATE_FAILED,
				StatusDetail: &v1alpha1.TaskStatusDetail{Reason: v1alpha1.TaskStatusReason_TASK_STATUS_REASON_PROVISIONING_FAILED},
			}},
			want: false,
		},
		{
			name: "failed_with_outcome",
			task: &v1alpha1.Task{Status: &v1alpha1.TaskStatus{
				State:   v1alpha1.TaskState_TASK_STATE_FAILED,
				Outcome: &v1alpha1.TaskOutcome{Result: v1alpha1.TaskResult_TASK_RESULT_FAILED, ExitCode: 1},
			}},
			want: true,
		},
		{
			name: "completed",
			task: &v1alpha1.Task{Status: &v1alpha1.TaskStatus{State: v1alpha1.TaskState_TASK_STATE_COMPLETED}},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsExecutionTerminal(tc.task); got != tc.want {
				t.Fatalf("IsExecutionTerminal() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClaimInMemoryRestoreAndInteractiveIdle(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()

	t.Run("in_memory_run_id_prevents_false_crash_on_restore", func(t *testing.T) {
		row := testTaskRow(true)
		row.RunnerTokenHash = HashToken("tok")
		row.RunnerTokenConfirmed = true
		row.ActiveRunID = "r-1"

		next, resp, err := Claim(row, &runnerv1.ClaimAssignmentRequest{
			InMemoryRunId:    "r-1",
			LastStartedRunId: "r-1",
			ChildAlive:       false,
		}, nil, "tok", nil, now)
		if err != nil {
			t.Fatalf("Claim() error = %v", err)
		}
		if next.Task.GetStatus().GetState() == v1alpha1.TaskState_TASK_STATE_FAILED {
			t.Fatalf("expected non-FAILED state when in_memory_run_id is set, got %+v", next.Task.GetStatus())
		}
		if resp.GetMode() != runnerv1.AssignmentMode_ASSIGNMENT_MODE_RUN {
			t.Fatalf("Claim() mode = %v, want RUN", resp.GetMode())
		}
	})

	t.Run("interactive_task_without_commands_or_goal_gets_idle", func(t *testing.T) {
		row := &store.TaskRow{
			Task: &v1alpha1.Task{
				Metadata: &v1alpha1.ObjectMeta{Atespace: "default", Name: "ssh-only"},
				Spec:     &v1alpha1.TaskSpec{},
				Status:   &v1alpha1.TaskStatus{State: v1alpha1.TaskState_TASK_STATE_QUEUED},
			},
			Runnable: true,
		}
		next, resp, err := Claim(row, &runnerv1.ClaimAssignmentRequest{Nonce: "n-1"}, nil, "", fixedTokenMinter("tok"), now)
		if err != nil {
			t.Fatalf("Claim() error = %v", err)
		}
		if resp.GetMode() != runnerv1.AssignmentMode_ASSIGNMENT_MODE_IDLE || next.ActiveRunID != "" {
			t.Fatalf("expected IDLE and empty ActiveRunID for interactive task, got mode=%v runID=%q", resp.GetMode(), next.ActiveRunID)
		}
	})
}

func TestApplyEventsDoesNotOverwriteCancelled(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	row := testTaskRow(false)
	row.RunnerTokenHash = HashToken("tok")
	row.ActiveRunID = "r-1"
	row.Task.Status.State = v1alpha1.TaskState_TASK_STATE_CANCELLED

	next, resp, park, err := ApplyEvents(row, &runnerv1.ReportEventsRequest{
		RunId: "r-1",
		Events: []*runnerv1.RunnerEvent{{
			Seq:   1,
			Event: &runnerv1.RunnerEvent_CommandExited{CommandExited: &runnerv1.CommandExited{ExitCode: 0}},
		}},
	}, "tok", now)
	if err != nil {
		t.Fatalf("ApplyEvents() error = %v", err)
	}
	if park || resp.GetAckSeq() != 1 || resp.GetNext().GetMode() != runnerv1.AssignmentMode_ASSIGNMENT_MODE_IDLE {
		t.Fatalf("unexpected response for cancelled task: park=%v resp=%+v", park, resp)
	}
	if next.Task.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_CANCELLED {
		t.Fatalf("expected CANCELLED to be preserved, got %v", next.Task.GetStatus().GetState())
	}
}
