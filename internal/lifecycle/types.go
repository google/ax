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

// Package lifecycle defines the pure, I/O-free Task and runner state machine
// contract shared by ax-server's gRPC (:8080) and runner (:8081) handlers.
package lifecycle

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/proto"
)

// ErrInvalidTransition indicates an operation is invalid for the task's
// current state (maps to gRPC FailedPrecondition).
var ErrInvalidTransition = errors.New("invalid task state transition")

// State bundles the persisted Task resource together with the runner control
// columns stored alongside it on store.TaskRow. All fields are value-copied or
// cloned by TransitionFunc implementations, which perform zero I/O.
//
// Note on sequence counters:
//   - RunSeq is a task-lifetime monotonic ID allocator ("r-1", "r-2", ...) that
//     NEVER resets across retries or cold restores.
//   - LastAckedRunnerSeq tracks the runner's per-execution event counter and
//     RESETS to 0 whenever a new command run starts.
type State struct {
	// Task is the persisted AX Task resource. Stored via store.StoredStatus so
	// status.state persists only AX-owned states (QUEUED, COMPLETED, FAILED,
	// CANCELLED), while live actor states (WORKING, SUSPENDED, SUSPENDING) and
	// status.actor / status.actor_uid continue to be overlaid from Agent
	// Substrate at read time.
	Task *v1alpha1.Task

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

// Clone returns a deep copy of s safe to mutate inside a TransitionFunc.
func (s State) Clone() State {
	out := s
	if s.Task != nil {
		out.Task = proto.Clone(s.Task).(*v1alpha1.Task)
	}
	if len(s.RunnerTokenHash) > 0 {
		out.RunnerTokenHash = append([]byte(nil), s.RunnerTokenHash...)
	} else {
		out.RunnerTokenHash = nil
	}
	return out
}

// NextRunID increments RunSeq and returns a monotonic run ID ("r-1", ...).
func (s *State) NextRunID() string {
	s.RunSeq++
	return fmt.Sprintf("r-%d", s.RunSeq)
}

// Output carries non-persisted results returned by a TransitionFunc to the
// caller (runner assignment payloads are added alongside pkg/apis/runner/v1).
type Output struct{}

// TransitionFunc computes the next task state for input in at timestamp now.
// Implementations must never mutate their input State in place and must perform
// zero network or storage I/O.
type TransitionFunc func(state State, in Input, now time.Time) (State, Output, error)

// Input is the sealed sum type of events that mutate a Task row.
//
// Pointer receivers implement Input so a single type switch handles every
// variant unambiguously. Runner claim and event Input variants (alongside
// Output assignment payloads) are added with the runner wire protocol in
// pkg/apis/runner/v1.
type Input interface {
	isInput()
}

// CreateInput initializes a new Task row at the start of CreateTask.
//
// On the synchronous path, CreateTask does not start tasks running immediately:
// CreateInput initializes the row in TASK_STATE_QUEUED with Runnable = false
// (replacing any client-supplied Task.Status) while ax-server synchronously
// provisions the shared ActorTemplate, warm Workspace Tag, Actor, and
// EgressPolicy and parks the new actor on Substrate; once parked, ax-server
// clears the stored state so GetTask/ListTasks report TASK_STATE_SUSPENDED
// live from Substrate until ResumeTask sets Runnable = true.
type CreateInput struct {
	// Task is the validated incoming Task manifest (any client-supplied Status
	// is replaced).
	Task *v1alpha1.Task
	// AteActorTemplate is the resolved Substrate ActorTemplate name stored on
	// Task.Status.AteActorTemplate (e.g. "ax-<sha256[:16]>").
	AteActorTemplate string
	// ResolvedArgv is the effective command argv (Task.Spec.Commands if set,
	// otherwise resolved from the container image ENTRYPOINT + CMD).
	ResolvedArgv []string
	// WorkspaceReady sets the initial WorkspaceReady condition to True (for
	// tasks with no workspace setup or an already-ready warm Workspace Tag).
	WorkspaceReady bool
}

func (*CreateInput) isInput() {}

// SuspendInput marks a task non-runnable for a user-initiated SuspendTask.
type SuspendInput struct{}

func (*SuspendInput) isInput() {}

// ResumeInput authorizes command execution on a non-terminal task for ResumeTask.
type ResumeInput struct{}

func (*ResumeInput) isInput() {}

// DeleteInput marks a task with metadata.delete_time while synchronous
// Substrate actor deletion is in progress.
type DeleteInput struct{}

func (*DeleteInput) isInput() {}

// IsTerminal reports whether state is COMPLETED, FAILED, or CANCELLED.
func IsTerminal(state v1alpha1.TaskState) bool {
	switch state {
	case v1alpha1.TaskState_TASK_STATE_COMPLETED,
		v1alpha1.TaskState_TASK_STATE_FAILED,
		v1alpha1.TaskState_TASK_STATE_CANCELLED:
		return true
	default:
		return false
	}
}
