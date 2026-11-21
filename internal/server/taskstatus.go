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

package server

import (
	"cmp"
	"context"
	"log/slog"
	"time"

	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"

	"github.com/google/ax/internal/store"
	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

const (
	// DefaultTaskStatusInterval is how often WatchTaskStatus polls the
	// runners of running tasks.
	DefaultTaskStatusInterval = 2 * time.Second
	// taskStatusTimeout bounds one poll of one runner.
	taskStatusTimeout = 5 * time.Second
	// parkTimeout bounds suspending the actor of a task that ended.
	parkTimeout = 30 * time.Second
	// reasonCompleted is the Ready condition's reason once a task completed.
	reasonCompleted = "Completed"
)

// WatchTaskStatus finds out when a task ends and blocks until ctx is done.
// Every interval it asks the runner of each task whose actor is running and
// whose execution is not over how the task ended (v1alpha1.RunnerStatusPath).
// A COMPLETED or FAILED status is recorded as the task's state and outcome,
// and the actor is suspended so the task no longer holds a worker.
//
// Every ax-server replica may run it: recording a status is an atomic
// transition that only the first replica makes, and parking holds the task's
// lock.
func (s *Server) WatchTaskStatus(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultTaskStatusInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkTaskStatus(ctx)
		}
	}
}

// checkTaskStatus polls the runners of running tasks once.
func (s *Server) checkTaskStatus(ctx context.Context) {
	// listTasks overlays each task's live state from its actor, so only tasks
	// whose actor is running read WORKING.
	tasks, err := s.listTasks(ctx, "", 0, 0)
	if err != nil {
		slog.Warn("listing tasks to check whether they ended", "error", err)
		return
	}
	byAtespace := make(map[string][]*v1alpha1.Task)
	for _, t := range tasks {
		if t.GetMetadata().GetDeleteTime() != nil || isExecutionTerminal(t) ||
			t.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_WORKING {
			continue
		}
		ns := cmp.Or(t.GetMetadata().GetAtespace(), v1alpha1.DefaultAtespace)
		byAtespace[ns] = append(byAtespace[ns], t)
	}
	for ns, group := range byAtespace {
		actors, err := s.substrate.ListActors(ctx, ns)
		if err != nil {
			slog.Warn("listing actors to check whether their tasks ended", "atespace", ns, "error", err)
			continue
		}
		byName := make(map[string]*ateapipb.Actor, len(actors))
		for _, a := range actors {
			byName[a.GetMetadata().GetName()] = a
		}
		for _, t := range group {
			name := t.GetMetadata().GetName()
			actor := byName[name]
			if actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_RUNNING {
				continue
			}
			pollCtx, cancel := context.WithTimeout(ctx, taskStatusTimeout)
			st, err := s.reconciler.TaskStatus(pollCtx, ns, name, substrate.WorkerIP(actor))
			cancel()
			if err != nil {
				slog.Debug("asking the runner whether the task ended", "atespace", ns, "task", name, "error", err)
				continue
			}
			if st != nil && (st.State == v1alpha1.RunnerStateCompleted || st.State == v1alpha1.RunnerStateFailed) {
				s.recordTaskEnd(ctx, ns, name, st)
			}
		}
	}
}

// recordTaskEnd records st as the task's state and outcome unless its
// execution is already over, and then suspends its actor.
func (s *Server) recordTaskEnd(ctx context.Context, atespace, name string, st *v1alpha1.RunnerStatus) {
	key := store.TaskKey{Atespace: atespace, Name: name}
	recorded := false
	_, err := s.applier.Apply(ctx, key, func(current *store.TaskVersion, _ time.Time) (*store.TaskVersion, error) {
		if current == nil || current.Task.GetMetadata().GetDeleteTime() != nil || isExecutionTerminal(current.Task) {
			return current, nil
		}
		next := current.Clone()
		applyTaskEnd(next.Task, st)
		recorded = true
		return next, nil
	})
	if err != nil {
		slog.Warn("recording how the task ended", "atespace", atespace, "task", name, "error", err)
		return
	}
	if !recorded {
		return
	}
	slog.Info("task ended", "atespace", atespace, "task", name, "state", st.State, "exitCode", st.ExitCode, "message", st.Message)
	s.parkEndedActor(ctx, atespace, name)
}

// applyTaskEnd sets task's state, outcome, and Ready condition from st, which
// is COMPLETED or FAILED.
func applyTaskEnd(task *v1alpha1.Task, st *v1alpha1.RunnerStatus) {
	if task.Status == nil {
		task.Status = &v1alpha1.TaskStatus{}
	}
	ts := task.Status
	ts.StatusDetail = nil
	outcome := &v1alpha1.TaskOutcome{
		ExitCode: st.ExitCode,
		Output:   st.Output,
	}
	if st.State == v1alpha1.RunnerStateCompleted {
		ts.State = v1alpha1.TaskState_TASK_STATE_COMPLETED
		outcome.Result = v1alpha1.TaskResult_TASK_RESULT_SUCCEEDED
		msg := cmp.Or(st.Message, "Task completed")
		if st.Message != "" {
			outcome.Status = &rpcstatus.Status{Code: int32(codes.OK), Message: st.Message}
		}
		ts.Outcome = outcome
		setReady(task, "False", reasonCompleted, msg)
		return
	}
	msg := cmp.Or(st.Message, "Task failed")
	ts.State = v1alpha1.TaskState_TASK_STATE_FAILED
	outcome.Result = v1alpha1.TaskResult_TASK_RESULT_FAILED
	outcome.Status = &rpcstatus.Status{Code: int32(codes.Unknown), Message: msg}
	ts.Outcome = outcome
	setReady(task, "False", v1alpha1.ReasonFailed, msg)
}

// parkEndedActor suspends the actor of a task whose execution is over. It
// holds the task's lock, so it waits for a ResumeTask, SuspendTask, or
// DeleteTask in progress, and then does nothing unless the task is still over
// and not being deleted.
func (s *Server) parkEndedActor(ctx context.Context, atespace, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), parkTimeout)
	defer cancel()
	unlock, err := s.locker.Lock(ctx, "task", atespace, name)
	if err != nil {
		slog.Warn("locking the ended task to suspend its actor", "atespace", atespace, "task", name, "error", err)
		return
	}
	defer unlock()
	row, err := s.applier.GetTaskVersion(ctx, store.TaskKey{Atespace: atespace, Name: name})
	if err != nil || row.Task.GetMetadata().GetDeleteTime() != nil || !isExecutionTerminal(row.Task) {
		return
	}
	if err := s.substrate.SuspendActor(ctx, atespace, name); err != nil {
		slog.Warn("suspending the ended task's actor", "atespace", atespace, "task", name, "error", err)
		return
	}
	slog.Info("suspended the ended task's actor", "atespace", atespace, "task", name)
}
