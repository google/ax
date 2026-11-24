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
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/ax/internal/store"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
)

// observeConcurrency caps how many runners ObserveStatus asks at once.
//
// TODO(#690): Optimize the concurrency for large fleets, together with
// observeInterval in cmd/ax-server: a pass must ask every running task's
// runner, each with a 2 s timeout, before the next one starts.
const observeConcurrency = 16

// ObserveStatus asks the runner of every running task (store.ListRunningTasks,
// then the actor's state on Agent Substrate) how the task ended, and stores
// the COMPLETED and FAILED ones as the tasks' states and outcomes. It repeats
// every interval until ctx is done. Reads take both from the store, so
// they need no sandbox, and suspending or resuming a task keeps the state.
func (s *Server) ObserveStatus(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.observeStatusOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// observeStatusOnce makes one pass of ObserveStatus over the running tasks of
// all atespaces.
func (s *Server) observeStatusOnce(ctx context.Context) {
	tasks, err := s.listRunningTasks(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("could not list the tasks to ask how they ended", "error", err)
		}
		return
	}
	sem := make(chan struct{}, observeConcurrency)
	var wg sync.WaitGroup
	for _, task := range tasks {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			s.observeTaskStatus(ctx, task)
		})
	}
	wg.Wait()
}

// observeTaskStatus asks the runner of task, a task that may be running, how
// the task ended and, once it reports COMPLETED or FAILED, stores that.
func (s *Server) observeTaskStatus(ctx context.Context, task *v1alpha1.Task) {
	atespace, name := task.GetMetadata().GetAtespace(), task.GetMetadata().GetName()
	// Only a running actor's runner is asked: a request to a suspended one
	// would not be answered.
	actor, err := s.substrate.GetActor(ctx, atespace, name)
	if err != nil || actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_RUNNING {
		return
	}
	rs, err := s.taskManager.RunnerStatus(ctx, atespace, name)
	if err != nil {
		return
	}
	state, reason := v1alpha1.TaskState_TASK_STATE_COMPLETED, v1alpha1.ReasonTaskCompleted
	switch rs.State {
	case v1alpha1.RunnerStateCompleted:
	case v1alpha1.RunnerStateFailed:
		state, reason = v1alpha1.TaskState_TASK_STATE_FAILED, v1alpha1.ReasonTaskFailed
	default:
		return
	}
	// The lock is taken only when there is an end to store.
	unlock, err := s.locker.Lock(ctx, "task", atespace, name)
	if err != nil {
		return
	}
	defer unlock()
	// The task may have changed, or been replaced, since it was read.
	uid := task.GetMetadata().GetUid()
	task, err = s.getStoredTask(ctx, atespace, name)
	if err != nil || !awaitsEnd(task) || task.GetMetadata().GetUid() != uid {
		return
	}
	task.Status.State = state
	// An outcome, even one without output, marks the task as ended.
	task.Status.Outcome = &v1alpha1.TaskOutcome{Output: rs.Output}
	setReady(task, "False", reason, "")
	if err := s.updateTask(ctx, atespace, name, task.Status); err != nil {
		slog.Warn("could not store how the task ended", "atespace", atespace, "task", name, "error", err)
	}
}

// awaitsEnd reports whether a stored task can still report how it ended: it
// has no state of AX's own, such as Completed, and is not being deleted.
func awaitsEnd(task *v1alpha1.Task) bool {
	return task.GetStatus() != nil && store.MayBeRunning(task)
}
