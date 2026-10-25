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
	"log/slog"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Task status ownership.
//
// A task's actor is the source of truth for whether the task is running, where,
// and whether it is healthy. AX does not keep a copy of that: the stored status
// holds only what AX itself knows (the task ID, workspace setup, and the phases
// v1alpha1.IsAXPhase accepts), and Observe fills in the rest from Substrate
// each time a task is read.

// Observe fills in the Substrate-owned parts of each task's status (actor,
// workerIP, phase and the Ready condition) from the task's actor. It costs one
// GetActor call for a single task and one ListActors call per atespace
// otherwise. If Substrate cannot be reached, the affected tasks are reported
// with phase "Unknown".
func (r *TaskReconciler) Observe(ctx context.Context, tasks []*v1alpha1.Task) {
	if len(tasks) == 1 {
		t := tasks[0]
		actor, err := r.client.GetActor(ctx, atespaceOf(t), t.GetMetadata().GetName())
		if err != nil && status.Code(err) == codes.NotFound {
			actor, err = nil, nil
		}
		applyActor(t, actor, err)
		return
	}

	byAtespace := make(map[string][]*v1alpha1.Task)
	for _, t := range tasks {
		byAtespace[atespaceOf(t)] = append(byAtespace[atespaceOf(t)], t)
	}
	for atespace, group := range byAtespace {
		actors, err := r.client.ListActors(ctx, atespace)
		byName := make(map[string]*ateapipb.Actor, len(actors))
		for _, a := range actors {
			byName[a.GetMetadata().GetName()] = a
		}
		for _, t := range group {
			applyActor(t, byName[t.GetMetadata().GetName()], err)
		}
	}
}

// applyActor overlays the actor's state on the task's status. actor is nil when
// the task has no actor; err is set when the actor could not be looked up.
func applyActor(task *v1alpha1.Task, actor *ateapipb.Actor, err error) {
	if task.Status == nil {
		task.Status = &v1alpha1.TaskStatus{}
	}
	st := task.Status
	st.Actor = ""
	st.WorkerIp = ""
	now := time.Now()

	switch {
	case err != nil:
		slog.Warn("could not observe task actor", "atespace", atespaceOf(task), "task", task.GetMetadata().GetName(), "error", err)
		if !v1alpha1.IsAXPhase(st.Phase) {
			st.Phase = "Unknown"
			setConditionIfChanged(task, condReady, "Unknown", "SubstrateUnavailable", err.Error(), now)
		}
	case actor == nil:
		// A task that finished provisioning always has an actor, so a missing one
		// was removed outside AX.
		if !v1alpha1.IsAXPhase(st.Phase) {
			st.Phase = v1alpha1.PhaseFailed
			setConditionIfChanged(task, condReady, "False", "ActorNotFound", "The task's actor no longer exists on Substrate", now)
		}
	default:
		st.Actor = actor.GetMetadata().GetName()
		st.WorkerIp = substrate.WorkerIP(actor)
		if v1alpha1.IsAXPhase(st.Phase) {
			return
		}
		phase, ready, reason, message := actorPhase(task, actor)
		st.Phase = phase
		setConditionIfChanged(task, condReady, ready, reason, message, now)
	}
}

// actorPhase maps an actor's state to the task's phase and Ready condition.
func actorPhase(task *v1alpha1.Task, actor *ateapipb.Actor) (phase, ready, reason, message string) {
	switch state := actor.GetStatus().GetState(); state {
	case ateapipb.ActorState_ACTOR_STATE_RUNNING:
		if task.GetStatus() != nil && conditionTrue(task, condWorkspaceReady) {
			return "Running", "True", "TaskRunning", "Task is running and its workspace is ready"
		}
		return "Running", "False", "WorkspaceInitializing", "Waiting for workspace setup to complete"
	case ateapipb.ActorState_ACTOR_STATE_RESUMING, ateapipb.ActorState_ACTOR_STATE_REVERTING:
		return "Resuming", "False", "ActorResuming", "Actor is being resumed"
	case ateapipb.ActorState_ACTOR_STATE_SUSPENDING:
		return "Suspending", "False", "ActorSuspending", "Actor is being suspended"
	case ateapipb.ActorState_ACTOR_STATE_SUSPENDED:
		return "Suspended", "False", "TaskSuspended", "Task is suspended"
	case ateapipb.ActorState_ACTOR_STATE_CRASHED:
		msg := actor.GetStatus().GetCrash().GetMessage()
		if msg == "" {
			msg = "Actor crashed"
		}
		return v1alpha1.PhaseFailed, "False", "ActorCrashed", msg
	case ateapipb.ActorState_ACTOR_STATE_DELETING:
		return v1alpha1.PhaseTerminating, "False", "ActorDeleting", "Actor is being deleted"
	default:
		return "Unknown", "Unknown", "UnknownActorState", state.String()
	}
}

// setConditionIfChanged sets a condition, keeping its update time when nothing
// about it changed so that repeated reads report a stable timestamp.
func setConditionIfChanged(task *v1alpha1.Task, condType, condStatus, reason, message string, t time.Time) {
	for _, c := range task.Status.Conditions {
		if c.Type == condType && c.Status == condStatus && c.Reason == reason && c.Message == message {
			return
		}
	}
	setCondition(task, condType, condStatus, reason, message, t)
}

func atespaceOf(task *v1alpha1.Task) string {
	if a := task.GetMetadata().GetAtespace(); a != "" {
		return a
	}
	return "default"
}
