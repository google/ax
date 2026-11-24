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

	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
)

// observeConcurrency caps how many runners ObserveStatus asks at once.
const observeConcurrency = 16

// ObserveStatus asks the runner of every running actor how its task ended, and
// stores the COMPLETED and FAILED ones as the tasks' states and outcomes. It
// repeats every interval until ctx is done. Reads take both from the store, so
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

// observeStatusOnce makes one pass of ObserveStatus over the running actors of
// all atespaces.
func (s *Server) observeStatusOnce(ctx context.Context) {
	actors, err := s.substrate.ListActors(ctx, "", ateapipb.ActorState_ACTOR_STATE_RUNNING)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("could not list the actors to ask how their tasks ended", "error", err)
		}
		return
	}
	sem := make(chan struct{}, observeConcurrency)
	var wg sync.WaitGroup
	for _, actor := range actors {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			s.observeTaskStatus(ctx, actor.GetMetadata().GetAtespace(), actor.GetMetadata().GetName())
		})
	}
	wg.Wait()
}

// observeTaskStatus asks the runner of the running actor named name how its
// task ended and, once it reports COMPLETED or FAILED, stores that.
func (s *Server) observeTaskStatus(ctx context.Context, atespace, name string) {
	// Not every actor is a task's, and a task that ended is not asked again.
	task, err := s.getStoredTask(ctx, atespace, name)
	if err != nil || !awaitsEnd(task) {
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
	return task.GetStatus() != nil && !v1alpha1.IsAXTaskState(task.Status.State) && task.GetMetadata().GetDeleteTime() == nil
}
