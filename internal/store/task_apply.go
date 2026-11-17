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
	"fmt"
	"time"

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

// TaskRow holds the persisted Task resource together with the row revision and
// pull-based runner control columns stored alongside it in the tasks table.
//
// Note on sequence counters:
//   - RunSeq is a task-lifetime monotonic ID allocator ("r-1", "r-2", ...) that
//     NEVER resets across retries or cold restores.
//   - LastAckedRunnerSeq tracks the runner's per-execution event counter and
//     RESETS to 0 whenever a new command run starts.
type TaskRow struct {
	// Task is the persisted AX Task resource. Stored via [StoredStatus] so
	// status.state persists only AX-owned states (QUEUED, COMPLETED, FAILED,
	// CANCELLED), while live actor states (WORKING, SUSPENDED, SUSPENDING) and
	// status.actor / status.actor_uid continue to be overlaid from Agent
	// Substrate at read time.
	Task *v1alpha1.Task

	// Generation is the monotonic row revision (+1 per committed Apply write;
	// never resets), mirrored onto Task.Status.Generation.
	Generation int64

	// ActorUID is the Substrate Actor.metadata.uid recorded when the task's
	// actor is created, binding runner requests (via the Actor JWT actorUID
	// claim) to the expected actor incarnation.
	ActorUID string

	// Runnable records whether ax-server currently authorizes ax-task-runner to
	// execute the task's command (false when newly created suspended, after
	// SuspendTask, or once terminal; true after ResumeTask). This allows
	// ClaimAssignment to return ASSIGNMENT_MODE_IDLE when a paused actor is
	// woken externally (e.g. via ax ssh) while keeping Task.Status.State
	// live-derived from Substrate.
	Runnable bool

	// ActiveRunID is the server-issued run ID ("r-1", "r-2", ...) currently
	// executing on ax-task-runner. Empty before the first run claim and once
	// terminal. Used to fence sequenced runner events.
	ActiveRunID string

	// RunSeq is the task-lifetime monotonic counter incremented by NextRunID to
	// mint unique ActiveRunID values ("r-1", "r-2", ...). Never resets.
	RunSeq int64

	// RunnerTokenHash is the SHA-256 digest of the per-actor runner token
	// minted on initial assignment claim (stored outside Task protojson so it is
	// never exposed by GetTask or ListTasks).
	RunnerTokenHash []byte

	// RunnerTokenConfirmed becomes true once the runner proves possession of
	// the minted token on a subsequent claim or sequenced event. While false,
	// retrying the initial claim with the matching ClaimNonce may re-mint the
	// token after a lost HTTP response.
	RunnerTokenConfirmed bool

	// ClaimNonce records the per-wakeup nonce from the initial token-minting
	// claim while RunnerTokenConfirmed is false, making lost-response claim
	// retries idempotent. Cleared once RunnerTokenConfirmed becomes true.
	ClaimNonce string

	// LastAckedRunnerSeq is the highest 1-based runner event sequence number
	// applied in the current ActiveRunID execution. Events at or below
	// LastAckedRunnerSeq are ignored as duplicates. Resets to 0 whenever a new
	// run assignment is issued; preserved across resume and idle assignments.
	LastAckedRunnerSeq int64
}

// Clone returns a deep copy of r safe to mutate inside an ApplyFunc.
func (r *TaskRow) Clone() *TaskRow {
	if r == nil {
		return nil
	}
	out := *r
	if r.Task != nil {
		out.Task = proto.Clone(r.Task).(*v1alpha1.Task)
	}
	if len(r.RunnerTokenHash) > 0 {
		out.RunnerTokenHash = append([]byte(nil), r.RunnerTokenHash...)
	} else {
		out.RunnerTokenHash = nil
	}
	return &out
}

// NextRunID increments RunSeq and returns a monotonic run ID ("r-1", ...).
// It returns "" if r is nil.
func (r *TaskRow) NextRunID() string {
	if r == nil {
		return ""
	}
	r.RunSeq++
	return fmt.Sprintf("r-%d", r.RunSeq)
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
// only pure in-memory transitions inside ApplyFunc:
//
//	written, err := applier.Apply(ctx, key, func(current *store.TaskRow, now time.Time) (*store.TaskRow, error) {
//		if current == nil {
//			return nil, store.ErrNotFound
//		}
//		next := current.Clone()
//		// mutate next in memory ...
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
