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

// Package lifecycle defines the pure, I/O-free Task state machine contract
// shared by ax-server's API/runner handlers and the background action queue and
// drift sweep reconciler.
package lifecycle

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/protobuf/proto"
)

const (
	// ClaimWatchdogTimeout is how long a resumed QUEUED task waits for
	// ax-task-runner to claim its assignment before TimerExpired fires.
	ClaimWatchdogTimeout = 5 * time.Minute

	// DefaultActionInitialBackoff is the sleep delay after the first retryable
	// action or capacity failure (before exponential growth).
	DefaultActionInitialBackoff = time.Second

	// DefaultActionMaxBackoff caps the per-attempt sleep interval between
	// individual action or capacity retries (1s -> 2s -> ... -> 60s), so a
	// queued task keeps polling at least once a minute while waiting on
	// transient errors or worker capacity.
	DefaultActionMaxBackoff = 60 * time.Second
)

// ErrInvalidTransition indicates an operation is invalid for the task's
// current state (maps to gRPC FailedPrecondition).
var ErrInvalidTransition = errors.New("invalid task state transition")

// ActionKind identifies one idempotent Agent Substrate operation stored in
// tasks.pending_actions.
type ActionKind string

const (
	ActionWaitWorkspaceTag ActionKind = "WAIT_WORKSPACE_TAG" // Poll/wait until a preparing Workspace snapshot Tag is ready.
	ActionEnsureTemplate   ActionKind = "ENSURE_TEMPLATE"    // Ensure the shared Profile ActorTemplate exists on Substrate.
	ActionCreateActor      ActionKind = "CREATE_ACTOR"       // Create the Task's Actor (from Action.Tag when non-empty).
	ActionEnsureEgress     ActionKind = "ENSURE_EGRESS"      // Bind the Actor's outbound EgressPolicy (runner + Gateway rules).
	ActionResumeActor      ActionKind = "RESUME_ACTOR"       // Resume the Actor onto a worker and wait for /readyz.
	ActionSuspendActorSync ActionKind = "SUSPEND_ACTOR_SYNC" // Synchronously checkpoint and suspend the Actor to durable storage.
	ActionRevertActor      ActionKind = "REVERT_ACTOR"       // Roll the Actor back to its latest durable snapshot (leaves it SUSPENDED).
	ActionDeleteActor      ActionKind = "DELETE_ACTOR"       // Delete the Actor from Substrate before removing the task row.
	ActionCreateTag        ActionKind = "CREATE_TAG"         // Publish a named Substrate Tag from the suspended Actor's snapshot.
	ActionDeleteTag        ActionKind = "DELETE_TAG"         // Delete a Substrate snapshot Tag.
)

// SuspendReason records why ActionSuspendActorSync was queued so SUSPENDING
// transitions to the intended post-checkpoint state without adding internal
// reasons to the public TaskStatusReason proto enum.
type SuspendReason string

const (
	SuspendReasonUser     SuspendReason = "user"      // SuspendTask -> TASK_STATE_SUSPENDED
	SuspendReasonTurnIdle SuspendReason = "turn_idle" // Idle notice -> TASK_STATE_IDLE
	SuspendReasonEviction SuspendReason = "eviction"  // Worker eviction -> TASK_STATE_QUEUED + RESUME_ACTOR
	SuspendReasonSetup    SuspendReason = "setup"     // Setup finished -> durable checkpoint + RESUME_ACTOR
	SuspendReasonTerminal SuspendReason = "terminal"  // Park actor after COMPLETED or FAILED
)

// Action is a single queued Agent Substrate operation serialized in
// tasks.pending_actions. Workers always execute the head element (Actions[0]).
type Action struct {
	// ID is the task-scoped monotonic identifier ("a-1", "a-2", ...) allocated
	// by State.NextAction. Workers echo ID in ActionCompletedInput /
	// ActionFailedInput / CapacityUnavailableInput; Transition ignores results
	// whose ActionID no longer matches State.Actions[0].ID, fencing out
	// completions of actions superseded while in flight (e.g. a RESUME_ACTOR
	// canceled by DeleteTask).
	ID string `json:"id"`

	// Kind specifies which idempotent Substrate operation to execute.
	Kind ActionKind `json:"kind"`

	// Tag is an optional Substrate snapshot Tag name:
	//   - on CREATE_ACTOR: source snapshot Tag (Actor.source_tag) to seed the
	//     actor from (empty boots from the ActorTemplate's golden snapshot);
	//   - on CREATE_TAG / DELETE_TAG: target snapshot Tag to create or delete.
	Tag string `json:"tag,omitempty"`

	// SuspendReason is set on SUSPEND_ACTOR_SYNC actions to record which target
	// state (SUSPENDED, IDLE, QUEUED, or terminal) Transition should enter once
	// the synchronous checkpoint completes.
	SuspendReason SuspendReason `json:"suspendReason,omitempty"`

	// Tries counts how many transient or capacity-exhausted attempts have failed
	// on this Action so far (0 on first enqueue). Used to compute exponential
	// retry backoff up to DefaultActionMaxBackoff.
	Tries int `json:"tries"`

	// Since is the timestamp when this Action was originally enqueued (set by
	// State.NextAction and preserved across retries) for observability and
	// queue-wait diagnostics.
	Since time.Time `json:"since"`

	// NotBefore is the earliest timestamp at which a worker may execute this
	// Action (zero means runnable immediately). Set to now + backoff on
	// capacity/transient retries or spec.retry crash-recovery delays;
	// NextReconcileAt schedules the row for max(now, Actions[0].NotBefore).
	NotBefore time.Time `json:"notBefore,omitempty"`
}

// State bundles the Task resource together with the control-plane runtime
// columns stored alongside it on store.TaskRow. All fields are value-copied or
// cloned by Transition; Transition performs zero I/O.
//
// Note on sequence counters:
//   - RunSeq and ActionSeq are task-lifetime monotonic ID allocators that NEVER
//     reset across retries, checkpoints, or reverts.
//   - LastAckedRunnerSeq tracks the runner's per-execution event counter and
//     RESETS to 0 whenever a new workspace setup pass or command run starts.
type State struct {
	// Task is the persisted AX Task resource. Live Substrate actor identity
	// fields (status.actor, status.actor_uid) are scrubbed before storage and
	// overlaid at read time.
	Task *v1alpha1.Task

	// Actions is the ordered queue of pending idempotent Agent Substrate
	// operations. Transition returns the complete replacement slice; workers
	// execute Actions[0] and fence completions on Actions[0].ID.
	Actions []Action

	// ActorUID is the Substrate Actor.metadata.uid recorded when CREATE_ACTOR
	// completes. Used to bind runner requests to the active actor incarnation
	// and ignore stale drift-sweep observations; cleared if the actor is
	// confirmed missing.
	ActorUID string

	// ActiveRunID is the server-issued run ID ("r-1", "r-2", ...) currently
	// executing on ax-task-runner. Empty before the first run claim (including
	// during workspace setup), while waiting for a crash-revert retry, and
	// once terminal. Used to fence sequenced runner events.
	ActiveRunID string

	// RunSeq is the task-lifetime monotonic counter incremented by NextRunID to
	// mint unique ActiveRunID values ("r-1", "r-2", ...). Never resets.
	RunSeq int64

	// ActionSeq is the task-lifetime monotonic counter incremented by
	// NextAction to mint unique Action.ID values ("a-1", "a-2", ...). Never
	// resets, preventing stale worker completions from matching later actions.
	ActionSeq int64

	// RunnerTokenHash is the SHA-256 digest of the per-actor runner token
	// minted on initial assignment claim. Resets to nil only when REVERT_ACTOR
	// rolls the actor back past RunnerTokenGen (wiping the token file inside
	// the sandbox) so the runner can mint a fresh token on next claim.
	RunnerTokenHash []byte

	// RunnerTokenConfirmed becomes true once the runner proves possession of
	// the minted token on a subsequent claim or sequenced event. While false on
	// an unstarted QUEUED task, retrying the initial claim with the same nonce
	// may re-mint the token after a lost HTTP response. Resets to false whenever
	// a new token is minted or RunnerTokenHash is cleared on revert.
	RunnerTokenConfirmed bool

	// RunnerTokenGen is the durable Substrate Snapshot.generation that first
	// captured the confirmed runner token (0 until a post-mint
	// SUSPEND_ACTOR_SYNC completes). If REVERT_ACTOR restores a snapshot with
	// generation < RunnerTokenGen (or when RunnerTokenGen == 0),
	// RunnerTokenHash, RunnerTokenConfirmed, and RunnerTokenGen all reset to
	// zero.
	RunnerTokenGen int32

	// LastAckedRunnerSeq is the highest 1-based runner event sequence number
	// applied in the current runner execution (a single workspace setup pass or
	// command run). Events at or below LastAckedRunnerSeq are ignored as
	// duplicates. Resets to 0 whenever Transition issues a new setup or run
	// assignment; preserved across resume and idle assignments.
	LastAckedRunnerSeq int64

	// ClaimDeadline is the watchdog timestamp (now + ClaimWatchdogTimeout) set
	// when RESUME_ACTOR finishes on a QUEUED task with no remaining actions (or
	// on external wakeup). Cleared once the runner claims, starts, suspends,
	// deletes, fails, or TimerExpired fires.
	ClaimDeadline *time.Time

	// LastResumedAt records when the task's actor most recently resumed. Used
	// alongside TaskRow.LastHeartbeatAt as the silence baseline for lost-runner
	// detection so newly woken sandboxes are given a full grace window.
	LastResumedAt *time.Time
}

// Clone returns a deep copy of s safe to mutate inside Transition.
func (s State) Clone() State {
	out := s
	if s.Task != nil {
		out.Task = proto.Clone(s.Task).(*v1alpha1.Task)
	}
	if len(s.Actions) > 0 {
		out.Actions = make([]Action, len(s.Actions))
		copy(out.Actions, s.Actions)
	} else {
		out.Actions = nil
	}
	if len(s.RunnerTokenHash) > 0 {
		out.RunnerTokenHash = append([]byte(nil), s.RunnerTokenHash...)
	} else {
		out.RunnerTokenHash = nil
	}
	if s.ClaimDeadline != nil {
		t := *s.ClaimDeadline
		out.ClaimDeadline = &t
	}
	if s.LastResumedAt != nil {
		t := *s.LastResumedAt
		out.LastResumedAt = &t
	}
	return out
}

// NextAction allocates a deterministic Action with a monotonic ID ("a-1", ...).
func (s *State) NextAction(kind ActionKind, now time.Time) Action {
	s.ActionSeq++
	return Action{
		ID:    fmt.Sprintf("a-%d", s.ActionSeq),
		Kind:  kind,
		Since: now,
	}
}

// NextRunID increments RunSeq and returns a monotonic run ID ("r-1", ...).
func (s *State) NextRunID() string {
	s.RunSeq++
	return fmt.Sprintf("r-%d", s.RunSeq)
}

// Output carries non-persisted results returned by Transition to the caller.
type Output struct {
	// DeleteRow indicates that the task's actor has been deleted from Agent
	// Substrate and the task row should now be removed from storage by
	// returning store.DeleteTaskRow from store.ApplyFunc.
	DeleteRow bool
}

// TransitionFunc computes the next task state and complete pending action list
// for input in at timestamp now.
//
// Contract invariants for implementations and callers:
//   - Pure & I/O-free: never mutates its input State in place, performs zero
//     I/O, returns the complete replacement State.Actions slice (preserving
//     head Action.ID when an in-flight action must remain valid), and sets
//     Output.DeleteRow = true only when ActionDeleteActor completes.
//   - Level-triggered observations: ObservedActorInput and HeartbeatLostInput
//     are idempotent across repeated sweeps (see their type docs).
type TransitionFunc func(state State, in Input, now time.Time) (State, Output, error)

// Input is the sealed sum type of every event that can mutate a Task.
//
// Pointer receivers implement Input so a single type switch in Transition
// handles every variant unambiguously without runtime multi-field validation.
// Variants are grouped by caller:
//   - gRPC API: *CreateInput, *SuspendInput, *ResumeInput, *DeleteInput
//   - Reconciler & drift sweep: *ActionCompletedInput, *ActionFailedInput,
//     *CapacityUnavailableInput, *ObservedActorInput, *HeartbeatLostInput,
//     *TimerExpiredInput
//
// Runner claim and event Input variants (alongside Output assignment payloads)
// are added by the Runner + ax-server workstream with the runner wire protocol.
type Input interface {
	isInput()
}

// --- gRPC API inputs ---

// CreateInput initializes a new Task in TASK_STATE_QUEUED.
//
// CreateTask does not support starting directly in TASK_STATE_SUSPENDED:
// TaskSpec has no initial-state field, any client-supplied Task.Status is
// replaced by Transition, and CreateInput always starts new tasks in
// TASK_STATE_QUEUED (to pause a task, callers invoke SuspendTask after
// creation).
type CreateInput struct {
	// Task is the validated incoming Task manifest (any client-supplied Status
	// is replaced by Transition).
	Task *v1alpha1.Task
	// AteActorTemplate is the resolved Substrate ActorTemplate name stored on
	// Task.Status.AteActorTemplate (e.g. "ax-<sha256[:16]>").
	AteActorTemplate string
	// ResolvedArgv is the effective command argv (Task.Spec.Commands if set,
	// otherwise resolved from the container image ENTRYPOINT + CMD).
	ResolvedArgv []string
	// SourceTag is the pre-warmed Workspace snapshot Tag used to seed
	// CREATE_ACTOR when already available (empty on cold boot or while waiting).
	SourceTag string
	// WorkspaceReady sets the initial WorkspaceReady condition to True (for
	// tasks with no workspace setup or an already-ready SourceTag).
	WorkspaceReady bool
	// WaitWorkspaceTag prepends ActionWaitWorkspaceTag ahead of provisioning
	// when the bound Workspace is still pre-warming its snapshot Tag.
	WaitWorkspaceTag bool
}

func (*CreateInput) isInput() {}

// SuspendInput requests a user-initiated checkpoint (SuspendTask).
type SuspendInput struct{}

func (*SuspendInput) isInput() {}

// ResumeInput wakes a SUSPENDED or IDLE task (ResumeTask).
type ResumeInput struct{}

func (*ResumeInput) isInput() {}

// DeleteInput marks a task for deletion and queues DELETE_ACTOR (DeleteTask).
type DeleteInput struct{}

func (*DeleteInput) isInput() {}

// --- Reconciler & drift sweep inputs ---

// ActionCompletedInput reports that the head pending action succeeded on Substrate.
type ActionCompletedInput struct {
	// ActionID is the Action.ID that completed; ignored if it no longer matches
	// State.Actions[0].ID.
	ActionID string
	// Kind is the completed Action.Kind.
	Kind ActionKind
	// ActorUID is the Substrate Actor.metadata.uid populated when CREATE_ACTOR completes.
	ActorUID string
	// SnapshotGen is the active durable Snapshot.generation reported by
	// Substrate when SUSPEND_ACTOR_SYNC or REVERT_ACTOR completes.
	SnapshotGen int32
	// SourceTag is populated when WAIT_WORKSPACE_TAG resolves a warm snapshot Tag.
	SourceTag string
}

func (*ActionCompletedInput) isInput() {}

// ActionFailedInput reports a permanent Substrate error on the head action.
type ActionFailedInput struct {
	// ActionID is the Action.ID that failed; ignored if no longer at State.Actions[0].
	ActionID string
	// Kind is the failed Action.Kind.
	Kind ActionKind
	// Reason classifies the failure on Task.Status.StatusDetail (defaults to
	// PROVISIONING_FAILED when unspecified).
	Reason v1alpha1.TaskStatusReason
	// Message is the human-readable failure detail surfaced on Task.Status.
	Message string
}

func (*ActionFailedInput) isInput() {}

// CapacityUnavailableInput reports RESOURCE_EXHAUSTED on RESUME_ACTOR, backing
// off the head action via Action.Tries / Action.NotBefore while keeping the
// task in TASK_STATE_QUEUED (never failing the task for capacity exhaustion).
type CapacityUnavailableInput struct {
	// ActionID is the RESUME_ACTOR Action.ID that received RESOURCE_EXHAUSTED.
	ActionID string
	// Message is the diagnostic message returned by Substrate.
	Message string
}

func (*CapacityUnavailableInput) isInput() {}

// ObservedActorInput reports actor state drift observed by the periodic sweep
// (or FAILED_PRECONDITION when suspending a crashed actor).
//
// Observations are level-triggered: the sweep reports the same drift on every
// pass until Substrate converges. Transition is therefore idempotent for a
// repeated observation: it does not consume another retry for a crash whose
// REVERT_ACTOR is still pending, does not replace an equivalent pending action
// (which would change the head action ID that workers fence completions on),
// and ignores MissingConfirmed until CREATE_ACTOR has completed.
type ObservedActorInput struct {
	// ActorState is the live ActorState observed on Substrate (ignored when
	// MissingConfirmed is true).
	ActorState ateapipb.ActorState
	// ActorUID is the UID of the observed actor on Substrate. If both ActorUID
	// and State.ActorUID are non-empty and differ, the observation belongs to a
	// different actor incarnation with the same name and is ignored.
	ActorUID string
	// CrashMessage is optional diagnostic detail from Substrate when ActorState
	// is ACTOR_STATE_CRASHED.
	CrashMessage string
	// MissingConfirmed is true when a targeted GetActor check confirmed the
	// actor no longer exists on Substrate (NotFound).
	MissingConfirmed bool
}

func (*ObservedActorInput) isInput() {}

// HeartbeatLostInput reports that a running task's runner exceeded the silence
// threshold. Transition ignores it unless the runner is expected to be alive
// (that is, the task is QUEUED or WORKING with no pending actions). Parked
// (IDLE, SUSPENDED), checkpointing, provisioning, and backing-off tasks are
// legitimately silent, so a sweep racing a state change cannot fail them.
type HeartbeatLostInput struct {
	// SilentFor is the elapsed duration since the latest runner heartbeat (or
	// LastResumedAt when no post-resume heartbeat has been recorded).
	SilentFor time.Duration
}

func (*HeartbeatLostInput) isInput() {}

// TimerExpiredInput fires when a due task has no ready pending actions (e.g.
// the 5-minute runner claim watchdog).
type TimerExpiredInput struct{}

func (*TimerExpiredInput) isInput() {}

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
