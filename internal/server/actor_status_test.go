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
	"errors"
	"testing"

	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func testActor(name string, state ateapipb.ActorState) *ateapipb.Actor {
	return &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "default", Name: name},
		Status:   &ateapipb.ActorStatus{State: state},
	}
}

func storedTask(name string, state v1alpha1.TaskState, conds ...*v1alpha1.Condition) *v1alpha1.Task {
	return &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: name, Atespace: "default"},
		Status:   &v1alpha1.TaskStatus{State: state, Conditions: conds},
	}
}

func readyCondition(task *v1alpha1.Task) *v1alpha1.Condition {
	for _, c := range task.GetStatus().GetConditions() {
		if c.GetType() == "Ready" {
			return c
		}
	}
	return nil
}

func TestSetActorStatus(t *testing.T) {
	workspaceReady := &v1alpha1.Condition{Type: "WorkspaceReady", Status: "True"}
	crashed := testActor("crashed", ateapipb.ActorState_ACTOR_STATE_CRASHED)
	crashed.Status.Crash = &ateapipb.ActorCrash{Message: "OOMKilled"}

	tests := []struct {
		task                       *v1alpha1.Task
		actor                      *ateapipb.Actor
		wantState                  v1alpha1.TaskState
		wantActor                  string
		wantReady, wantReadyReason string
	}{
		{storedTask("running", v1alpha1.TaskState_TASK_STATE_UNSPECIFIED, workspaceReady), testActor("running", ateapipb.ActorState_ACTOR_STATE_RUNNING), v1alpha1.TaskState_TASK_STATE_WORKING, "running", "True", v1alpha1.ReasonTaskRunning},
		{storedTask("setting-up", v1alpha1.TaskState_TASK_STATE_UNSPECIFIED), testActor("setting-up", ateapipb.ActorState_ACTOR_STATE_RUNNING), v1alpha1.TaskState_TASK_STATE_WORKING, "setting-up", "False", v1alpha1.ReasonWorkspaceInitializing},
		{storedTask("suspended", v1alpha1.TaskState_TASK_STATE_UNSPECIFIED, workspaceReady), testActor("suspended", ateapipb.ActorState_ACTOR_STATE_SUSPENDED), v1alpha1.TaskState_TASK_STATE_SUSPENDED, "suspended", "False", v1alpha1.ReasonSuspended},
		{storedTask("crashed", v1alpha1.TaskState_TASK_STATE_UNSPECIFIED), crashed, v1alpha1.TaskState_TASK_STATE_FAILED, "crashed", "False", v1alpha1.ReasonFailed},
		{storedTask("reverting", v1alpha1.TaskState_TASK_STATE_UNSPECIFIED), testActor("reverting", ateapipb.ActorState_ACTOR_STATE_REVERTING), v1alpha1.TaskState_TASK_STATE_QUEUED, "reverting", "False", v1alpha1.ReasonResuming},
		{storedTask("suspending", v1alpha1.TaskState_TASK_STATE_UNSPECIFIED), testActor("suspending", ateapipb.ActorState_ACTOR_STATE_SUSPENDING), v1alpha1.TaskState_TASK_STATE_SUSPENDING, "suspending", "False", v1alpha1.ReasonSuspending},
		{storedTask("deleting", v1alpha1.TaskState_TASK_STATE_UNSPECIFIED), testActor("deleting", ateapipb.ActorState_ACTOR_STATE_DELETING), v1alpha1.TaskState_TASK_STATE_UNSPECIFIED, "deleting", "False", v1alpha1.ReasonTerminating},
		{storedTask("odd", v1alpha1.TaskState_TASK_STATE_UNSPECIFIED), testActor("odd", ateapipb.ActorState_ACTOR_STATE_UNSPECIFIED), v1alpha1.TaskState_TASK_STATE_UNSPECIFIED, "odd", "Unknown", v1alpha1.ReasonUnknown},
		// The actor was removed outside AX.
		{storedTask("gone", v1alpha1.TaskState_TASK_STATE_UNSPECIFIED), nil, v1alpha1.TaskState_TASK_STATE_FAILED, "", "False", v1alpha1.ReasonActorNotFound},
		// States that describe AX's own work are kept, and so is their Ready reason.
		{storedTask("failed", v1alpha1.TaskState_TASK_STATE_FAILED, &v1alpha1.Condition{Type: "Ready", Status: "False", Reason: v1alpha1.ReasonActorResumeFailed}), testActor("failed", ateapipb.ActorState_ACTOR_STATE_SUSPENDED), v1alpha1.TaskState_TASK_STATE_FAILED, "failed", "False", v1alpha1.ReasonActorResumeFailed},
		{storedTask("not-yet", v1alpha1.TaskState_TASK_STATE_QUEUED), nil, v1alpha1.TaskState_TASK_STATE_QUEUED, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.task.GetMetadata().GetName(), func(t *testing.T) {
			setActorStatus(tt.task, tt.actor, nil)
			st := tt.task.GetStatus()
			if st.GetState() != tt.wantState || st.GetActor() != tt.wantActor {
				t.Errorf("state=%v actor=%q, want %v %q", st.GetState(), st.GetActor(), tt.wantState, tt.wantActor)
			}
			if tt.wantReady == "" {
				return
			}
			if c := readyCondition(tt.task); c.GetStatus() != tt.wantReady || c.GetReason() != tt.wantReadyReason {
				t.Errorf("Ready = %v, want %s/%s", c, tt.wantReady, tt.wantReadyReason)
			}
		})
	}

	t.Run("crash message and status detail", func(t *testing.T) {
		task := storedTask("crashed", v1alpha1.TaskState_TASK_STATE_UNSPECIFIED)
		setActorStatus(task, crashed, nil)
		if msg := readyCondition(task).GetMessage(); msg != "OOMKilled" {
			t.Errorf("Ready message = %q, want the actor's crash message", msg)
		}
		if detail := task.GetStatus().GetStatusDetail(); detail.GetReason() != v1alpha1.TaskStatusReason_TASK_STATUS_REASON_SANDBOX_CRASHED || detail.GetDetail() != "OOMKilled" {
			t.Errorf("StatusDetail = %v, want SANDBOX_CRASHED/OOMKilled", detail)
		}
	})

	t.Run("stable update time", func(t *testing.T) {
		task := storedTask("suspended", v1alpha1.TaskState_TASK_STATE_UNSPECIFIED)
		setActorStatus(task, testActor("suspended", ateapipb.ActorState_ACTOR_STATE_SUSPENDED), nil)
		first := readyCondition(task).GetUpdateTime()
		setActorStatus(task, testActor("suspended", ateapipb.ActorState_ACTOR_STATE_SUSPENDED), nil)
		if got := readyCondition(task).GetUpdateTime(); got != first {
			t.Errorf("Ready update time changed on an unchanged read")
		}
	})
}

func TestSetActorStatus_SubstrateErrorReportsUnknown(t *testing.T) {
	err := errors.New("denied")
	task := storedTask("a", v1alpha1.TaskState_TASK_STATE_UNSPECIFIED)
	terminating := storedTask("b", v1alpha1.TaskState_TASK_STATE_UNSPECIFIED)
	terminating.Metadata.DeleteTime = timestamppb.Now()
	setActorStatus(task, nil, err)
	setActorStatus(terminating, nil, err)

	if got := task.GetStatus().GetState(); got != v1alpha1.TaskState_TASK_STATE_UNSPECIFIED {
		t.Errorf("state = %v, want UNSPECIFIED", got)
	}
	if c := readyCondition(task); c.GetStatus() != "Unknown" || c.GetReason() != v1alpha1.ReasonSubstrateUnavailable {
		t.Errorf("Ready = %v, want Unknown/SubstrateUnavailable", c)
	}
	if got := v1alpha1.FormatTaskState(terminating); got != v1alpha1.ReasonTerminating {
		t.Errorf("FormatTaskState = %q, want %q", got, v1alpha1.ReasonTerminating)
	}
	if c := readyCondition(terminating); c.GetStatus() != "False" || c.GetReason() != v1alpha1.ReasonTerminating {
		t.Errorf("Ready = %v, want False/Terminating", c)
	}
}

func TestSetActorStatus_ActorUID(t *testing.T) {
	actor := testActor("a", ateapipb.ActorState_ACTOR_STATE_RUNNING)
	actor.Metadata.Uid = "uid-new"
	for _, tt := range []struct {
		name  string
		actor *ateapipb.Actor
		err   error
		want  string
	}{
		{"current actor", actor, nil, "uid-new"},
		{"actor without UID", testActor("a", ateapipb.ActorState_ACTOR_STATE_RUNNING), nil, ""},
		{"missing actor", nil, nil, ""},
		{"unreachable substrate", nil, errors.New("unavailable"), ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			task := storedTask("a", v1alpha1.TaskState_TASK_STATE_UNSPECIFIED)
			task.Status.ActorUid = "uid-old"
			setActorStatus(task, tt.actor, tt.err)
			if got := task.GetStatus().GetActorUid(); got != tt.want {
				t.Errorf("actor UID = %q, want %q", got, tt.want)
			}
		})
	}
}
