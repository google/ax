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

// TaskRow holds the persisted Task resource and runner control state together
// with the storage-owned row revision.
//
// Embedding [lifecycle.State] promotes Task, ActorUID, Runnable, ActiveRunID,
// RunSeq, RunnerTokenHash, RunnerTokenConfirmed, ClaimNonce, and
// LastAckedRunnerSeq directly onto TaskRow while keeping storage-only fields
// out of [lifecycle.TransitionFunc].
type TaskRow struct {
	lifecycle.State

	// Generation is the monotonic row revision (+1 per committed Apply write;
	// never resets), mirrored onto Task.Status.Generation.
	Generation int64
}

// Clone returns a deep copy of r safe to mutate inside an ApplyFunc.
func (r *TaskRow) Clone() *TaskRow {
	if r == nil {
		return nil
	}
	out := *r
	out.State = r.State.Clone()
	return &out
}

// LifecycleState returns a deep copy of the pure [lifecycle.State] view of r.
func (r *TaskRow) LifecycleState() lifecycle.State {
	if r == nil {
		return lifecycle.State{}
	}
	return r.State.Clone()
}

// SetLifecycleState writes s onto r, scrubbing Task.Status via [StoredStatus]
// so Substrate-owned live fields and non-AX states are never persisted. It is a
// no-op if r is nil.
func (r *TaskRow) SetLifecycleState(s lifecycle.State) {
	if r == nil {
		return
	}
	c := s.Clone()
	if c.Task != nil {
		c.Task.Status = StoredStatus(c.Task.Status)
	}
	r.State = c
}

// StoredStatus returns the part of a task's status that AX owns and stores. The
// actor name, actor UID, and (for states outside [v1alpha1.IsAXTaskState]) the
// state, status detail, and Ready condition that follow from the actor's live
// state on Agent Substrate are dropped. st is not modified; a nil st returns an
// empty non-nil TaskStatus.
func StoredStatus(st *v1alpha1.TaskStatus) *v1alpha1.TaskStatus {
	out := &v1alpha1.TaskStatus{}
	if st != nil {
		out = proto.Clone(st).(*v1alpha1.TaskStatus)
	}
	out.Actor = ""
	out.ActorUid = ""
	if !v1alpha1.IsAXTaskState(out.State) {
		out.State = v1alpha1.TaskState_TASK_STATE_UNSPECIFIED
		out.StatusDetail = nil
		conds := out.Conditions[:0]
		for _, c := range out.Conditions {
			if c.GetType() != v1alpha1.ConditionReady {
				conds = append(conds, c)
			}
		}
		out.Conditions = conds
	}
	return out
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
//   - scrubs Task.Status via [StoredStatus] (initializing Task.Status if nil),
//     assigns Generation (1 on insert, stored Generation + 1 on update), and
//     mirrors Generation onto Task.Status.Generation;
//   - preserves the task's saved_at position in ListTasks on updates (unlike
//     SaveTask, which moves updated tasks to the front).
type ApplyFunc func(current *TaskRow, now time.Time) (*TaskRow, error)

// TaskApplier is the transactional write interface for task rows shared by
// ax-server's gRPC (:8080) and pull-based runner (:8081) handlers.
//
// Perform any external reads or Agent Substrate RPCs outside Apply, and run
// pure [lifecycle.TransitionFunc] mutations inside ApplyFunc:
//
//	written, err := applier.Apply(ctx, key, func(current *store.TaskRow, now time.Time) (*store.TaskRow, error) {
//		if current == nil {
//			return nil, store.ErrNotFound
//		}
//		nextState, _, err := transition(current.LifecycleState(), in, now)
//		if err != nil {
//			return nil, err
//		}
//		next := current.Clone()
//		next.SetLifecycleState(nextState)
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
	Apply(ctx context.Context, key TaskKey, fn ApplyFunc) (*TaskRow, error)
	// GetTaskRow returns the row for key without locking it, or [ErrNotFound]
	// if no such task exists.
	GetTaskRow(ctx context.Context, key TaskKey) (*TaskRow, error)
}
