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

package store

import (
	"context"
	"time"

	"github.com/google/ax/internal/lifecycle"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/protobuf/proto"
)

// TaskKey uniquely identifies a Task within an atespace.
type TaskKey struct {
	Atespace string
	Name     string
}

// Normalized returns k with an empty Atespace defaulted to DefaultAtespace.
func (k TaskKey) Normalized() TaskKey {
	if k.Atespace == "" {
		k.Atespace = v1alpha1.DefaultAtespace
	}
	return k
}

// TaskRow holds the persisted Task lifecycle state together with the
// provisioning, queue-lease, and liveness fields owned by storage.
//
// Embedding [lifecycle.State] promotes Task, Actions, ActorUID, ActiveRunID,
// RunSeq, ActionSeq, RunnerToken*, LastAckedRunnerSeq, ClaimDeadline, and
// LastResumedAt directly onto TaskRow while keeping storage-only fields out of
// [lifecycle.TransitionFunc].
type TaskRow struct {
	lifecycle.State

	EffectiveProfile *v1alpha1.Profile      // Resolved compute Profile snapshot at CreateTask.
	EgressRules      []*ateapipb.EgressRule // Snapped Gateway egress rules at CreateTask.
	Generation       int64                  // Monotonic row revision (+1 per write; never resets).
	LastHeartbeatAt  *time.Time             // Updated by batched heartbeat flushes.
	NextReconcileAt  *time.Time             // Queue wakeup timestamp; nil when task is at rest.
	LeaseOwner       string                 // Active queue worker lease holder ("<worker>:<token>").
	LeaseExpiresAt   *time.Time
}

// Clone returns a deep copy of r safe to mutate inside an ApplyFunc.
func (r *TaskRow) Clone() *TaskRow {
	if r == nil {
		return nil
	}
	out := *r
	out.State = r.State.Clone()
	if r.EffectiveProfile != nil {
		out.EffectiveProfile = proto.Clone(r.EffectiveProfile).(*v1alpha1.Profile)
	}
	if len(r.EgressRules) > 0 {
		out.EgressRules = make([]*ateapipb.EgressRule, len(r.EgressRules))
		for i, rule := range r.EgressRules {
			if rule != nil {
				out.EgressRules[i] = proto.Clone(rule).(*ateapipb.EgressRule)
			}
		}
	} else {
		out.EgressRules = nil
	}
	out.LastHeartbeatAt = cloneTimePtr(r.LastHeartbeatAt)
	out.NextReconcileAt = cloneTimePtr(r.NextReconcileAt)
	out.LeaseExpiresAt = cloneTimePtr(r.LeaseExpiresAt)
	return &out
}

// LifecycleState returns a deep copy of the pure [lifecycle.State] view of r.
func (r *TaskRow) LifecycleState() lifecycle.State {
	if r == nil {
		return lifecycle.State{}
	}
	return r.State.Clone()
}

// SetLifecycleState writes s onto r and recomputes NextReconcileAt at now.
// It is a no-op if r is nil. Live Substrate actor identity fields
// (status.actor and status.actor_uid) are scrubbed so they are never persisted.
func (r *TaskRow) SetLifecycleState(s lifecycle.State, now time.Time) {
	if r == nil {
		return
	}
	c := s.Clone()
	ScrubStoredTask(c.Task)
	r.State = c
	r.NextReconcileAt = lifecycle.NextReconcileAt(c, now)
}

// ScrubStoredTask clears live Substrate-owned fields on t.Status (actor and
// actor_uid) that must only be overlaid at read time (see TaskStatus in
// pkg/apis/v1alpha1/task.proto).
func ScrubStoredTask(t *v1alpha1.Task) {
	if t != nil && t.Status != nil {
		t.Status.Actor = ""
		t.Status.ActorUid = ""
	}
}

func cloneTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}

// DeleteTaskRow is a sentinel value that an [ApplyFunc] returns to delete a
// task row.
//
// Because row deletion is irreversible, Apply never infers it from nil.
// Returning nil leaves the row unchanged, whereas returning DeleteTaskRow
// deletes the row. This prevents an early no-op return (return nil, nil) from
// accidentally destroying a task.
//
// Apply recognizes the sentinel by pointer identity, so callers must return
// the variable itself:
//
//	return store.DeleteTaskRow, nil
//
// Passing a copy or clone instead of DeleteTaskRow itself will not match the
// sentinel; Apply will treat it as an ordinary TaskRow, reject its nil Task
// field, and return an error without modifying storage.
//
// In normal operation, only the handler for a completed DELETE_ACTOR action
// should return DeleteTaskRow: once Agent Substrate confirms that the task's
// actor has been deleted, lifecycle.Transition sets Output.DeleteRow, and the
// caller's ApplyFunc returns DeleteTaskRow to remove the database row in the
// same transaction.
var DeleteTaskRow = &TaskRow{}

// ApplyFunc computes an atomic state transition for a single task row.
//
// Apply invokes it inside a short transaction holding the row lock. current is
// the stored row (or nil if the row does not exist), and now is the store's
// current timestamp.
//
// The returned row controls how Apply mutates storage:
//
//	Returned row          Row missing                  Row exists
//	-------------------   --------------------------   --------------------------
//	nil                   no change                    no change
//	current (as passed)   (not possible)               no change
//	DeleteTaskRow         no change                    row is deleted
//	any other row         inserted at Generation 1     updated at Generation + 1
//
// If ApplyFunc returns a non-nil error, Apply aborts the write and returns that
// error unchanged.
//
// Rules for an ApplyFunc:
//
//   - Treat current as read-only. To modify the row, call
//     next := current.Clone(), edit next, and return next. Returning current
//     itself always leaves storage unchanged, even if current was mutated in place.
//   - To delete the row, return [DeleteTaskRow]. Returning nil never deletes.
//   - Any non-sentinel row returned for writing must have a non-nil Task field;
//     otherwise Apply returns an error without modifying storage.
//   - Do not perform network I/O, as the row lock is held until ApplyFunc returns.
//   - Keep the function safe to retry: if two callers race to create the same
//     task, the losing caller's ApplyFunc is invoked first with current == nil
//     and then retried with the row inserted by the winner.
//
// When inserting or updating a row, Apply also:
//
//   - overwrites Task.Metadata.Atespace and Task.Metadata.Name with the
//     normalized TaskKey passed to Apply;
//   - assigns Generation (1 on insert, stored Generation + 1 on update),
//     initializes Task.Status if nil, and mirrors Generation onto
//     Task.Status.Generation;
//   - preserves the task's saved_at position in ListTasks (unlike SaveTask,
//     which moves updated tasks to the front);
//   - clears any expired worker lease (LeaseExpiresAt <= now);
//   - clamps NextReconcileAt to at least LeaseExpiresAt while an active lease
//     is held, preventing another worker from claiming the row early.
type ApplyFunc func(current *TaskRow, now time.Time) (*TaskRow, error)

// TaskApplier is the transactional write interface for task rows. New task
// lifecycle code mutates tasks exclusively through Apply; the legacy [Store]
// task write methods (SaveTask, UpdateTaskStatus, SetTaskDeleteTime,
// DeleteTask) remain only until the gRPC Task handlers migrate to Apply.
//
// # Usage with lifecycle.TransitionFunc
//
// Perform any external reads (Workspace, Gateway, OCI image config, or Agent
// Substrate RPCs) BEFORE calling Apply, then run the pure
// [lifecycle.TransitionFunc] inside ApplyFunc using the store's now timestamp:
//
//	// 1. Creating a new task (ax-server CreateTask):
//	written, err := applier.Apply(ctx, key, func(current *store.TaskRow, now time.Time) (*store.TaskRow, error) {
//		if current != nil {
//			return nil, ErrAlreadyExists
//		}
//		nextState, _, err := lifecycle.Transition(lifecycle.State{}, &lifecycle.CreateInput{
//			Task:             task,
//			AteActorTemplate: tmplName,
//			ResolvedArgv:     resolvedArgv,
//			SourceTag:        sourceTag,
//			WorkspaceReady:   wsReady,
//		}, now)
//		if err != nil {
//			return nil, err
//		}
//		row := &store.TaskRow{
//			EffectiveProfile: effProfile,
//			EgressRules:      egressRules,
//		}
//		row.SetLifecycleState(nextState, now)
//		return row, nil
//	})
//
//	// 2. Advancing an existing task (API/runner handler, queue worker, or drift sweep):
//	written, err := applier.Apply(ctx, key, func(current *store.TaskRow, now time.Time) (*store.TaskRow, error) {
//		if current == nil {
//			return nil, store.ErrNotFound // or (nil, nil) in background reconcilers if already deleted
//		}
//		// Background callers can fence stale writes here before mutating:
//		//   - drift sweep:  if current.Generation != observedGen { return nil, nil }
//		//   - queue worker: verify current.LeaseOwner matches its active lease
//		nextState, out, err := lifecycle.Transition(current.LifecycleState(), in, now)
//		if err != nil {
//			return nil, err
//		}
//		if out.DeleteRow {
//			return store.DeleteTaskRow, nil
//		}
//		next := current.Clone()
//		next.SetLifecycleState(nextState, now)
//		return next, nil
//	})
type TaskApplier interface {
	// Apply executes fn under the row lock for key and commits the resulting
	// mutation (see [ApplyFunc]).
	//
	// On return:
	//
	//   - after an insert or update, it returns a copy of the row written;
	//   - when fn leaves an existing row unchanged, it returns the stored row
	//     without any in-place edits fn may have made to current;
	//   - when no row exists after commit (either because fn returned
	//     [DeleteTaskRow] or because the row was missing and fn returned nil),
	//     it returns (nil, nil);
	//   - when fn returns a non-nil error, it leaves storage unchanged and
	//     returns (nil, err).
	//
	// If acquiring the row lock or executing the transaction exceeds the store
	// timeout, Apply returns [ErrLockTimeout], which callers should treat as
	// retryable.
	Apply(ctx context.Context, key TaskKey, fn ApplyFunc) (*TaskRow, error)
	// GetTaskRow returns the row for key without locking it, or [ErrNotFound]
	// if no such task exists.
	GetTaskRow(ctx context.Context, key TaskKey) (*TaskRow, error)
}
