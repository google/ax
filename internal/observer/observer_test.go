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

package observer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/ax/internal/controller"
	"github.com/google/ax/internal/lock"
	"github.com/google/ax/internal/server"
	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/internal/substrate/substratetest"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
)

// fakeRunner accepts the task push, reports its workspace ready, and serves
// status on v1alpha1.RunnerStatusPath.
type fakeRunner struct {
	status atomic.Pointer[v1alpha1.RunnerStatus]
	reads  atomic.Int32
}

// observed is a server whose task "t" runs in an actor with a fake runner, and
// an Observer of its store.
type observed struct {
	*server.Server
	store *memory.MemoryStore
	o     *Observer
}

// newRunningTask returns a server whose task "t" runs in an actor with a fake
// runner, next to a running actor that is not a task's.
func newRunningTask(t *testing.T) (*observed, *fakeRunner) {
	t.Helper()
	runner := &fakeRunner{}
	runner.status.Store(&v1alpha1.RunnerStatus{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+v1alpha1.RunnerStatusPath, func(w http.ResponseWriter, r *http.Request) {
		if target := r.Header.Get("ate-target-actor"); target != "default/t" {
			t.Errorf("asked the runner of %q for its status, want only the task's", target)
		}
		runner.reads.Add(1)
		_ = json.NewEncoder(w).Encode(runner.status.Load())
	})
	// The task push and the workspace check.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	router := httptest.NewServer(mux)
	t.Cleanup(router.Close)
	t.Setenv("ATENET_ROUTER_ADDR", router.URL)

	sub := substratetest.NewServer(t)
	sub.SetActor(&ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "default", Name: "not-a-task"},
		Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
	})
	mem := memory.NewStore()
	locker := lock.NewMemoryLocker()
	tasks := controller.NewTaskManager(sub.Client(), "test-template", v1alpha1.DefaultSystemNamespace)
	srv := &observed{
		Server: server.NewServer(mem, tasks, server.Options{Locker: locker}),
		store:  mem,
		o:      New(mem, mem, locker, tasks),
	}
	ctx := context.Background()
	if _, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "t"},
		Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
	}}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if _, err := srv.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Name: "t"}); err != nil {
		t.Fatalf("ResumeTask: %v", err)
	}
	return srv, runner
}

func getTestTask(t *testing.T, srv *observed) *v1alpha1.Task {
	t.Helper()
	task, err := srv.GetTask(context.Background(), &v1alpha1.GetTaskRequest{Name: "t"})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	return task
}

// A pass asks the runner of a running task how the task ended, and stores
// Completed or Failed with the outcome. After that the runner is not asked
// again, and suspending and resuming the task keep the state.
func TestObserver(t *testing.T) {
	for _, tc := range []struct {
		reported   v1alpha1.RunnerStatus
		wantState  v1alpha1.TaskState
		wantReason string
	}{
		{
			reported:   v1alpha1.RunnerStatus{State: v1alpha1.RunnerStateCompleted, Output: "the answer"},
			wantState:  v1alpha1.TaskState_TASK_STATE_COMPLETED,
			wantReason: v1alpha1.ReasonTaskCompleted,
		},
		{
			reported:   v1alpha1.RunnerStatus{State: v1alpha1.RunnerStateFailed, Output: "tests failed: 3 of 120"},
			wantState:  v1alpha1.TaskState_TASK_STATE_FAILED,
			wantReason: v1alpha1.ReasonTaskFailed,
		},
	} {
		t.Run(tc.reported.State, func(t *testing.T) {
			srv, runner := newRunningTask(t)
			ctx := context.Background()
			check := func(what string, task *v1alpha1.Task) {
				t.Helper()
				status := task.GetStatus()
				if status.GetState() != tc.wantState {
					t.Errorf("%s: state = %v, want %v", what, status.GetState(), tc.wantState)
				}
				if o := status.GetOutcome(); o == nil || o.GetOutput() != tc.reported.Output {
					t.Errorf("%s: outcome = %v, want one with the reported output", what, o)
				}
				var ready *v1alpha1.Condition
				for _, c := range status.GetConditions() {
					if c.GetType() == v1alpha1.ConditionReady {
						ready = c
					}
				}
				if ready.GetStatus() != "False" || ready.GetReason() != tc.wantReason {
					t.Errorf("%s: Ready = %v, want False (%s)", what, ready, tc.wantReason)
				}
			}

			// Until the runner reports an end, the task is Running.
			srv.o.once(ctx)
			if task := getTestTask(t, srv); task.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_RUNNING || task.GetStatus().GetOutcome() != nil {
				t.Fatalf("status = %v, want RUNNING without an outcome", task.GetStatus())
			}

			runner.status.Store(&tc.reported)
			srv.o.once(ctx)
			check("GetTask", getTestTask(t, srv))
			stored, err := srv.store.GetTask(ctx, "default", "t")
			if err != nil {
				t.Fatalf("store.GetTask: %v", err)
			}
			check("store", stored)

			suspended, err := srv.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{Name: "t"})
			if err != nil {
				t.Fatalf("SuspendTask: %v", err)
			}
			check("suspended", suspended)
			resumed, err := srv.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Name: "t"})
			if err != nil {
				t.Fatalf("ResumeTask: %v", err)
			}
			check("resumed", resumed)
			srv.o.once(ctx)
			check("after another pass", getTestTask(t, srv))
			if got := runner.reads.Load(); got != 2 {
				t.Errorf("the runner was asked %d times, want 2: not again after the task ended", got)
			}
		})
	}
}

// A pass asks only the runners of running actors: not a suspended task's, and
// not an actor that is not a task's.
func TestObserver_AsksOnlyRunningTasks(t *testing.T) {
	srv, runner := newRunningTask(t)
	ctx := context.Background()
	if _, err := srv.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{Name: "t"}); err != nil {
		t.Fatalf("SuspendTask: %v", err)
	}
	runner.status.Store(&v1alpha1.RunnerStatus{State: v1alpha1.RunnerStateCompleted})
	srv.o.once(ctx)
	if got := runner.reads.Load(); got != 0 {
		t.Errorf("the suspended task's runner was asked %d times, want 0", got)
	}
	if got := getTestTask(t, srv).GetStatus().GetState(); got != v1alpha1.TaskState_TASK_STATE_SUSPENDED {
		t.Errorf("state = %v, want SUSPENDED", got)
	}
}

// run makes a pass every interval until its context is done.
func TestObserver_Repeats(t *testing.T) {
	srv, runner := newRunningTask(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.o.run(ctx, time.Millisecond)
	}()
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}

	// The end is reported only after a pass has found none.
	waitFor("the first pass", func() bool { return runner.reads.Load() > 0 })
	runner.status.Store(&v1alpha1.RunnerStatus{State: v1alpha1.RunnerStateCompleted})
	waitFor("a later pass to store the end", func() bool {
		return getTestTask(t, srv).GetStatus().GetState() == v1alpha1.TaskState_TASK_STATE_COMPLETED
	})

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after its context was done")
	}
}

// ResumeTask queues a task as working. Recording its end removes it, and
// resuming the ended task does not add it again.
func TestObserver_WorkingTasks(t *testing.T) {
	srv, runner := newRunningTask(t)
	ctx := context.Background()
	check := func(what string, want int) {
		t.Helper()
		got, err := srv.store.Claim(ctx)
		if err != nil {
			t.Fatalf("%s: Claim: %v", what, err)
		}
		if len(got) != want {
			t.Errorf("%s: working tasks = %v, want %d", what, got, want)
		}
	}
	check("resumed", 1)
	srv.o.once(ctx)
	check("no end yet", 1)
	runner.status.Store(&v1alpha1.RunnerStatus{State: v1alpha1.RunnerStateCompleted})
	srv.o.once(ctx)
	check("ended", 0)
	if _, err := srv.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Name: "t"}); err != nil {
		t.Fatalf("ResumeTask: %v", err)
	}
	check("resumed after the end", 0)
}

// Deleting a working task removes it from the queue.
func TestObserver_DeleteTask(t *testing.T) {
	srv, runner := newRunningTask(t)
	ctx := context.Background()
	if _, err := srv.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Name: "t"}); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	if got, err := srv.store.Claim(ctx); err != nil || len(got) != 0 {
		t.Errorf("Claim = %v, %v; want none", got, err)
	}
	srv.o.once(ctx)
	if got := runner.reads.Load(); got != 0 {
		t.Errorf("the deleted task's runner was asked %d times, want 0", got)
	}
}
