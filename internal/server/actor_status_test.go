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
	"go.ate.dev/substrate/pkg/proto/ateapipb"
)

func testActor(name string, state ateapipb.ActorState, workerIP string) *ateapipb.Actor {
	a := &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "default", Name: name},
		Status:   &ateapipb.ActorStatus{State: state},
	}
	if workerIP != "" {
		a.Status.WorkerAssignment = &ateapipb.WorkerAssignment{WorkerPodIps: []string{workerIP}}
	}
	return a
}

func storedTask(name, phase string, conds ...*v1alpha1.Condition) *v1alpha1.Task {
	return &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: name, Atespace: "default"},
		Status:   &v1alpha1.TaskStatus{Phase: phase, Conditions: conds},
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
	crashed := testActor("crashed", ateapipb.ActorState_ACTOR_STATE_CRASHED, "")
	crashed.Status.Crash = &ateapipb.ActorCrash{Message: "OOMKilled"}

	tests := []struct {
		task                         *v1alpha1.Task
		actor                        *ateapipb.Actor
		wantPhase, wantIP, wantActor string
		wantReady, wantReadyReason   string
	}{
		{storedTask("running", "", workspaceReady), testActor("running", ateapipb.ActorState_ACTOR_STATE_RUNNING, "10.0.0.5"), "Running", "10.0.0.5", "running", "True", v1alpha1.ReasonTaskRunning},
		{storedTask("setting-up", ""), testActor("setting-up", ateapipb.ActorState_ACTOR_STATE_RUNNING, "10.0.0.6"), "Running", "10.0.0.6", "setting-up", "False", v1alpha1.ReasonWorkspaceInitializing},
		{storedTask("suspended", "", workspaceReady), testActor("suspended", ateapipb.ActorState_ACTOR_STATE_SUSPENDED, ""), "Suspended", "", "suspended", "False", v1alpha1.PhaseSuspended},
		{storedTask("crashed", ""), crashed, v1alpha1.PhaseFailed, "", "crashed", "False", v1alpha1.PhaseFailed},
		{storedTask("reverting", ""), testActor("reverting", ateapipb.ActorState_ACTOR_STATE_REVERTING, ""), v1alpha1.PhaseResuming, "", "reverting", "False", v1alpha1.PhaseResuming},
		{storedTask("suspending", ""), testActor("suspending", ateapipb.ActorState_ACTOR_STATE_SUSPENDING, ""), v1alpha1.PhaseSuspending, "", "suspending", "False", v1alpha1.PhaseSuspending},
		{storedTask("deleting", ""), testActor("deleting", ateapipb.ActorState_ACTOR_STATE_DELETING, ""), v1alpha1.PhaseTerminating, "", "deleting", "False", v1alpha1.PhaseTerminating},
		{storedTask("odd", ""), testActor("odd", ateapipb.ActorState_ACTOR_STATE_UNSPECIFIED, ""), v1alpha1.PhaseUnknown, "", "odd", "Unknown", v1alpha1.PhaseUnknown},
		// The actor was removed outside AX.
		{storedTask("gone", ""), nil, v1alpha1.PhaseFailed, "", "", "False", v1alpha1.ReasonActorNotFound},
		// Phases that describe AX's own work are kept, and so is their Ready reason.
		{storedTask("failed", v1alpha1.PhaseFailed, &v1alpha1.Condition{Type: "Ready", Status: "False", Reason: v1alpha1.ReasonActorResumeFailed}), testActor("failed", ateapipb.ActorState_ACTOR_STATE_SUSPENDED, ""), v1alpha1.PhaseFailed, "", "failed", "False", v1alpha1.ReasonActorResumeFailed},
		{storedTask("not-yet", v1alpha1.PhasePending), nil, v1alpha1.PhasePending, "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.task.GetMetadata().GetName(), func(t *testing.T) {
			setActorStatus(tt.task, tt.actor, nil)
			st := tt.task.GetStatus()
			if st.GetPhase() != tt.wantPhase || st.GetWorkerIp() != tt.wantIP || st.GetActor() != tt.wantActor {
				t.Errorf("phase=%q workerIP=%q actor=%q, want %q %q %q", st.GetPhase(), st.GetWorkerIp(), st.GetActor(), tt.wantPhase, tt.wantIP, tt.wantActor)
			}
			if tt.wantReady == "" {
				return
			}
			if c := readyCondition(tt.task); c.GetStatus() != tt.wantReady || c.GetReason() != tt.wantReadyReason {
				t.Errorf("Ready = %v, want %s/%s", c, tt.wantReady, tt.wantReadyReason)
			}
		})
	}

	t.Run("crash message", func(t *testing.T) {
		task := storedTask("crashed", "")
		setActorStatus(task, crashed, nil)
		if msg := readyCondition(task).GetMessage(); msg != "OOMKilled" {
			t.Errorf("Ready message = %q, want the actor's crash message", msg)
		}
	})

	t.Run("stable update time", func(t *testing.T) {
		task := storedTask("suspended", "")
		setActorStatus(task, testActor("suspended", ateapipb.ActorState_ACTOR_STATE_SUSPENDED, ""), nil)
		first := readyCondition(task).GetUpdateTime()
		setActorStatus(task, testActor("suspended", ateapipb.ActorState_ACTOR_STATE_SUSPENDED, ""), nil)
		if got := readyCondition(task).GetUpdateTime(); got != first {
			t.Errorf("Ready update time changed on an unchanged read")
		}
	})
}

func TestSetActorStatus_SubstrateErrorReportsUnknown(t *testing.T) {
	err := errors.New("denied")
	task := storedTask("a", "")
	terminating := storedTask("b", v1alpha1.PhaseTerminating)
	setActorStatus(task, nil, err)
	setActorStatus(terminating, nil, err)

	if got := task.GetStatus().GetPhase(); got != "Unknown" {
		t.Errorf("phase = %q, want Unknown", got)
	}
	if c := readyCondition(task); c.GetStatus() != "Unknown" || c.GetReason() != v1alpha1.ReasonSubstrateUnavailable {
		t.Errorf("Ready = %v, want Unknown/SubstrateUnavailable", c)
	}
	if got := terminating.GetStatus().GetPhase(); got != v1alpha1.PhaseTerminating {
		t.Errorf("phase = %q, want %q to be kept", got, v1alpha1.PhaseTerminating)
	}
}
