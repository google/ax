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

package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	ateapipb "go.ate.dev/substrate/api/ateapi/v1"

	"github.com/google/ax/internal/server"
	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/internal/substrate/substratetest"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

// fakeStatusRunner stands in for a runner that accepts a pushed task and
// reports status, which is nil until the test sets it.
type fakeStatusRunner struct {
	mu     sync.Mutex
	status *v1alpha1.RunnerStatus
}

func (f *fakeStatusRunner) set(st v1alpha1.RunnerStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = &st
}

func (f *fakeStatusRunner) start(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+v1alpha1.RunnerTaskPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET "+v1alpha1.RunnerStatusPath, func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		st := f.status
		f.mu.Unlock()
		if st == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(st)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func TestWatchTaskStatus(t *testing.T) {
	cases := []struct {
		name       string
		status     v1alpha1.RunnerStatus
		wantState  v1alpha1.TaskState
		wantResult v1alpha1.TaskResult
		wantReady  string // Ready condition's message
		wantParked bool
	}{
		{
			name:      "completed",
			status:    v1alpha1.RunnerStatus{State: v1alpha1.RunnerStateCompleted, Output: "the answer"},
			wantState: v1alpha1.TaskState_TASK_STATE_COMPLETED, wantResult: v1alpha1.TaskResult_TASK_RESULT_SUCCEEDED,
			wantReady: "Task completed", wantParked: true,
		},
		{
			name:      "failed",
			status:    v1alpha1.RunnerStatus{State: v1alpha1.RunnerStateFailed, Message: "tests failed: 3 of 120", ExitCode: 1, Output: "log"},
			wantState: v1alpha1.TaskState_TASK_STATE_FAILED, wantResult: v1alpha1.TaskResult_TASK_RESULT_FAILED,
			wantReady: "tests failed: 3 of 120", wantParked: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ATENET_ROUTER_ADDR", "")
			runner := &fakeStatusRunner{}
			sub := substratetest.NewServer(t)
			sub.SetWorkerIP(runner.start(t))
			srv := server.NewServer(memory.NewStore(), newTestReconciler(sub))
			ctx := t.Context()

			if _, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
				Metadata: &v1alpha1.ObjectMeta{Name: "ends"},
				Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}, Commands: []string{"true"}},
			}}); err != nil {
				t.Fatalf("CreateTask: %v", err)
			}
			if _, err := srv.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Name: "ends"}); err != nil {
				t.Fatalf("ResumeTask: %v", err)
			}

			// While the command runs, nothing changes.
			srv.CheckTaskStatus(ctx)
			if got, _ := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "ends"}); got.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_WORKING {
				t.Fatalf("state before the task ended = %v, want WORKING", got.GetStatus().GetState())
			}

			runner.set(tc.status)
			srv.CheckTaskStatus(ctx)
			got, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "ends"})
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			st := got.GetStatus()
			if st.GetState() != tc.wantState {
				t.Fatalf("state = %v, want %v (status %+v)", st.GetState(), tc.wantState, st)
			}
			actor, err := sub.Client().GetActor(ctx, "default", "ends")
			if err != nil {
				t.Fatalf("GetActor: %v", err)
			}
			if parked := actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_SUSPENDED; parked != tc.wantParked {
				t.Errorf("actor state = %v, want suspended=%v", actor.GetStatus().GetState(), tc.wantParked)
			}
			if tc.wantResult == v1alpha1.TaskResult_TASK_RESULT_UNSPECIFIED {
				return
			}
			out := st.GetOutcome()
			if out.GetResult() != tc.wantResult || out.GetExitCode() != tc.status.ExitCode || out.GetOutput() != tc.status.Output {
				t.Errorf("outcome = %+v, want result %v, exit code %d, output %q", out, tc.wantResult, tc.status.ExitCode, tc.status.Output)
			}
			if ready := readyCondition(got); ready.GetMessage() != tc.wantReady {
				t.Errorf("Ready = %+v, want message %q", ready, tc.wantReady)
			}

			// A later poll, from this or another replica, changes nothing.
			runner.set(v1alpha1.RunnerStatus{State: v1alpha1.RunnerStateCompleted, Output: "other"})
			srv.CheckTaskStatus(ctx)
			if again, _ := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "ends"}); again.GetStatus().GetOutcome().GetOutput() != tc.status.Output {
				t.Errorf("outcome changed after the task ended: %+v", again.GetStatus().GetOutcome())
			}
		})
	}
}
