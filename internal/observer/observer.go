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

// Package observer records how tasks ended: it asks the runners of the
// working tasks for their status files and stores the COMPLETED and FAILED
// ones as the tasks' states and outcomes. cmd/ax-observer runs it.
package observer

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/ax/internal/controller"
	"github.com/google/ax/internal/lock"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// interval is how often Run asks the runners of the working tasks, those
// whose end is not recorded yet, how the tasks ended.
//
// TODO(#690): Optimize the interval: a shorter one reports an end sooner, but
// asks every working task's runner more often.
const interval = 5 * time.Second

// concurrency caps how many runners Run asks at once.
//
// TODO(#690): Optimize the concurrency for large fleets, together with
// interval: a pass reads the actor of every working task and asks the runner
// of each running one, with a 2 s timeout, before the next pass starts.
const concurrency = 16

// Observer asks the runners of the tasks in a store's working queue how the
// tasks ended. ax-server's ResumeTask adds a task to the queue, and the task
// leaves it when its end is recorded or the task is deleted.
type Observer struct {
	store     store.Store
	queue     store.WorkingQueue
	locker    lock.Locker
	tasks     *controller.TaskManager
	substrate *substrate.Client
}

// New returns an Observer of the tasks in st, whose working queue is queue.
// locker must be the one ax-server takes task locks with.
func New(st store.Store, queue store.WorkingQueue, locker lock.Locker, tasks *controller.TaskManager) *Observer {
	return &Observer{store: st, queue: queue, locker: locker, tasks: tasks, substrate: tasks.Client()}
}

// Run asks the runner of every running task whose end is not recorded yet how
// the task ended, and stores the COMPLETED and FAILED ones as the tasks'
// states and outcomes. It repeats every few seconds until ctx is done. Reads
// take both from the store, so they need no sandbox, and suspending or
// resuming a task keeps the state.
func (o *Observer) Run(ctx context.Context) {
	o.run(ctx, interval)
}

// run is Run with a pass every interval.
func (o *Observer) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		o.once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// once makes one pass of Run over the working tasks, asking the runners of
// those whose actors are running.
func (o *Observer) once(ctx context.Context) {
	tasks, err := o.queue.Claim(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("could not list the working tasks to ask how they ended", "error", err)
		}
		return
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, t := range tasks {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			// A suspended task's runner is not asked: asking would resume it.
			actor, err := o.substrate.GetActor(ctx, t.GetAtespace(), t.GetName())
			if err != nil || actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_RUNNING {
				return
			}
			o.observeTask(ctx, t)
		})
	}
	wg.Wait()
}

// observeTask asks the runner of a working task how the task ended and, once
// it reports COMPLETED or FAILED, stores that and removes the task from the
// working queue.
func (o *Observer) observeTask(ctx context.Context, ref *v1alpha1.ObjectRef) {
	atespace, name := ref.GetAtespace(), ref.GetName()
	task, err := o.store.GetTask(ctx, atespace, name)
	if err != nil {
		return
	}
	if task.GetStatus().GetOutcome() != nil {
		// The end was stored, but the task was not removed from the queue.
		if err := o.queue.Delete(ctx, ref); err != nil {
			slog.Warn("could not remove a task that ended from the working queue", "atespace", atespace, "task", name, "error", err)
		}
		return
	}
	if !awaitsEnd(task) {
		return
	}
	rs, err := o.tasks.RunnerStatus(ctx, atespace, name)
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
	unlock, err := o.locker.Lock(ctx, "task", atespace, name)
	if err != nil {
		return
	}
	defer unlock()
	// The task may have changed, or been replaced, since it was read.
	uid := task.GetMetadata().GetUid()
	task, err = o.store.GetTask(ctx, atespace, name)
	if err != nil || !awaitsEnd(task) || task.GetMetadata().GetUid() != uid {
		return
	}
	// The stored status holds nothing of the actor's, and Completed and
	// Failed are AX's own states, so it is stored as it is.
	task.Status.State = state
	// An outcome, even one without output, marks the task as ended.
	task.Status.Outcome = &v1alpha1.TaskOutcome{Output: rs.Output}
	setReady(task.Status, reason)
	if err := o.store.UpdateTaskStatus(ctx, atespace, name, task.Status); err != nil {
		slog.Warn("could not store how the task ended", "atespace", atespace, "task", name, "error", err)
		return
	}
	// A task that is not removed here is removed on its next pass.
	if err := o.queue.Delete(ctx, ref); err != nil {
		slog.Warn("could not remove a task that ended from the working queue", "atespace", atespace, "task", name, "error", err)
	}
}

// awaitsEnd reports whether a stored task can still report how it ended: it
// has no state of AX's own, such as Completed, and is not being deleted.
func awaitsEnd(task *v1alpha1.Task) bool {
	return task.GetStatus() != nil && !v1alpha1.IsAXTaskState(task.Status.State) && task.GetMetadata().GetDeleteTime() == nil
}

// setReady sets the Ready condition of a task that ended to False with reason.
// The stored status of a task without a state of AX's own has no Ready
// condition, but one is replaced if it is there.
func setReady(st *v1alpha1.TaskStatus, reason string) {
	conds := st.Conditions[:0]
	for _, c := range st.Conditions {
		if c.GetType() != v1alpha1.ConditionReady {
			conds = append(conds, c)
		}
	}
	st.Conditions = append(conds, &v1alpha1.Condition{
		Type:       v1alpha1.ConditionReady,
		Status:     "False",
		Reason:     reason,
		UpdateTime: timestamppb.Now(),
	})
}
