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

package controller

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/ax/internal/store"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

const (
	defaultWorkerGroup = "ax-controllers"
	// readRetryDelay is how long the worker waits after a transient error from the
	// event queue before trying again.
	readRetryDelay = time.Second
	// slotQueueSize is how many events may wait for one busy slot before the worker
	// stops reading. It keeps a task with several queued events from stalling the
	// other slots while bounding how much the worker holds unacknowledged.
	slotQueueSize = 64
)

// Worker consumes task events from the store's event queue and reconciles each
// task against Substrate. Run several with the same group name to share the load;
// every event is handled by exactly one of them.
type Worker struct {
	store      store.Store
	reconciler *TaskReconciler
	group      string
	consumer   string

	// Concurrency is how many events the worker reconciles at once. Events for the
	// same task always go to the same slot, in order, so a task is never reconciled
	// concurrently with itself. Values below 1 mean 1.
	Concurrency int
}

// NewWorker creates a worker that joins group as consumer. An empty group uses the
// default controller group; an empty consumer derives a unique name from the host.
func NewWorker(s store.Store, reconciler *TaskReconciler, group, consumer string) *Worker {
	if group == "" {
		group = defaultWorkerGroup
	}
	if consumer == "" {
		hostname, _ := os.Hostname()
		consumer = fmt.Sprintf("%s-%d", hostname, time.Now().UnixNano()%10000)
	}
	return &Worker{
		store:      s,
		reconciler: reconciler,
		group:      group,
		consumer:   consumer,
	}
}

// Run subscribes to task events and processes them until ctx is done. It returns
// ctx.Err() on shutdown, after in-flight events finish; every event is acknowledged
// after processing, even when reconciliation fails, so a bad task cannot wedge the queue.
func (w *Worker) Run(ctx context.Context) error {
	concurrency := max(w.Concurrency, 1)
	slog.Info("starting AX task worker", "group", w.group, "consumer", w.consumer, "concurrency", concurrency)

	sub, err := w.store.Subscribe(ctx, w.group, w.consumer)
	if err != nil {
		return fmt.Errorf("subscribing to task events: %w", err)
	}
	defer sub.Close()

	// Each slot is a goroutine with its own queue. Hashing the task key to a slot
	// keeps per-task ordering while unrelated tasks reconcile in parallel.
	slots := make([]chan store.TaskEvent, concurrency)
	var wg sync.WaitGroup
	for i := range slots {
		slots[i] = make(chan store.TaskEvent, slotQueueSize)
		wg.Add(1)
		go func(events <-chan store.TaskEvent) {
			defer wg.Done()
			for ev := range events {
				w.handle(ctx, sub, ev)
			}
		}(slots[i])
	}
	defer func() {
		for _, ch := range slots {
			close(ch)
		}
		wg.Wait()
	}()

	for {
		ev, err := sub.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				slog.Info("stopping AX task worker")
				return ctx.Err()
			}
			slog.Error("error reading task events", "error", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(readRetryDelay):
			}
			continue
		}

		select {
		case slots[slotFor(ev, concurrency)] <- ev:
		case <-ctx.Done():
			// Unacknowledged, so the event stays pending for the group.
			return ctx.Err()
		}
	}
}

// handle reconciles one event and acknowledges it.
func (w *Worker) handle(ctx context.Context, sub store.Subscription, ev store.TaskEvent) {
	if err := w.processEvent(ctx, ev); err != nil {
		slog.Error("error processing task event",
			"id", ev.ID,
			"atespace", ev.Atespace,
			"name", ev.Name,
			"action", ev.Action,
			"error", err,
		)
	}
	if err := sub.Ack(ctx, ev); err != nil {
		slog.Warn("failed to acknowledge task event", "id", ev.ID, "error", err)
	}
}

// slotFor maps an event to a worker slot by task, so every event for a given
// task is processed by the same slot.
func slotFor(ev store.TaskEvent, n int) int {
	if n <= 1 {
		return 0
	}
	h := fnv.New32a()
	h.Write([]byte(ev.Atespace))
	h.Write([]byte{0})
	h.Write([]byte(ev.Name))
	return int(h.Sum32() % uint32(n))
}

func (w *Worker) processEvent(ctx context.Context, ev store.TaskEvent) error {
	if ev.Action == "delete" {
		slog.Info("handling task deletion event", "atespace", ev.Atespace, "name", ev.Name)
		if err := w.reconciler.ReconcileDelete(ctx, ev.Atespace, ev.Name); err != nil {
			// Leave the record in Terminating so the failure is visible; re-running
			// `ax delete` republishes the event and retries the cleanup.
			return fmt.Errorf("cleaning up task %s/%s: %w", ev.Atespace, ev.Name, err)
		}
		if err := w.store.DeleteTask(ctx, ev.Atespace, ev.Name); err != nil {
			return fmt.Errorf("removing task record %s/%s: %w", ev.Atespace, ev.Name, err)
		}
		return nil
	}

	task, err := w.store.GetTask(ctx, ev.Atespace, ev.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			slog.Info("task not found, skipping reconcile", "atespace", ev.Atespace, "name", ev.Name)
			return nil
		}
		return fmt.Errorf("fetching task %s/%s: %w", ev.Atespace, ev.Name, err)
	}

	// Resolve every bound workspace. A missing one is skipped so the task still
	// runs; the runner creates an empty directory at its path.
	var workspaces []*v1alpha1.Workspace
	for _, ref := range task.Spec.WorkspaceRefs() {
		if ref.Name == "" {
			continue
		}
		wsp, err := w.store.GetWorkspace(ctx, task.Metadata.Atespace, ref.Name)
		if err == nil {
			workspaces = append(workspaces, wsp)
		} else if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("error fetching workspace", "name", ref.Name, "error", err)
		}
	}

	reconciled, err := w.reconciler.Reconcile(ctx, task, workspaces...)
	if err != nil {
		task.Status.Phase = "Failed"
		_ = w.store.UpdateTaskStatus(ctx, task.Metadata.Atespace, task.Metadata.Name, task.Status)
		return fmt.Errorf("reconciling task %s/%s: %w", task.Metadata.Atespace, task.Metadata.Name, err)
	}

	if err := w.store.UpdateTaskStatus(ctx, task.Metadata.Atespace, task.Metadata.Name, reconciled.Status); err != nil {
		return fmt.Errorf("updating task status %s/%s: %w", task.Metadata.Atespace, task.Metadata.Name, err)
	}

	return nil
}
