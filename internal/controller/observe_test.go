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

package controller_test

import (
	"context"
	"net"
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/ax/internal/controller"
	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// actorsServer serves a fixed set of actors, keyed by "atespace/name".
type actorsServer struct {
	ateapipb.UnimplementedControlServer
	actors    map[string]*ateapipb.Actor
	err       error
	getCalls  int
	listCalls int
}

func (a *actorsServer) GetActor(_ context.Context, req *ateapipb.GetActorRequest) (*ateapipb.Actor, error) {
	a.getCalls++
	if a.err != nil {
		return nil, a.err
	}
	if actor, ok := a.actors[req.GetActor().GetAtespace()+"/"+req.GetActor().GetName()]; ok {
		return actor, nil
	}
	return nil, status.Error(codes.NotFound, "actor not found")
}

func (a *actorsServer) ListActors(_ context.Context, req *ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
	a.listCalls++
	if a.err != nil {
		return nil, a.err
	}
	resp := &ateapipb.ListActorsResponse{}
	for _, actor := range a.actors {
		if actor.GetMetadata().GetAtespace() == req.GetAtespace() {
			resp.Actors = append(resp.Actors, actor)
		}
	}
	return resp, nil
}

func newObserver(t *testing.T, srv *actorsServer) *controller.TaskReconciler {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	g := grpc.NewServer()
	ateapipb.RegisterControlServer(g, srv)
	go g.Serve(lis)
	t.Cleanup(g.Stop)

	client, err := substrate.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("substrate client: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return controller.NewTaskReconciler(client, "", "", "")
}

func actor(atespace, name string, state ateapipb.ActorState, workerIP string) *ateapipb.Actor {
	a := &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: atespace, Name: name},
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

func TestObserve(t *testing.T) {
	workspaceReady := &v1alpha1.Condition{Type: "WorkspaceReady", Status: "True"}
	crashed := actor("default", "crashed", ateapipb.ActorState_ACTOR_STATE_CRASHED, "")
	crashed.Status.Crash = &ateapipb.ActorCrash{Message: "OOMKilled"}
	srv := &actorsServer{actors: map[string]*ateapipb.Actor{
		"default/running":    actor("default", "running", ateapipb.ActorState_ACTOR_STATE_RUNNING, "10.0.0.5"),
		"default/setting-up": actor("default", "setting-up", ateapipb.ActorState_ACTOR_STATE_RUNNING, "10.0.0.6"),
		"default/suspended":  actor("default", "suspended", ateapipb.ActorState_ACTOR_STATE_SUSPENDED, ""),
		"default/crashed":    crashed,
		"default/failed":     actor("default", "failed", ateapipb.ActorState_ACTOR_STATE_SUSPENDED, ""),
	}}
	r := newObserver(t, srv)

	tests := []struct {
		task                         *v1alpha1.Task
		wantPhase, wantIP, wantActor string
		wantReady, wantReadyReason   string
	}{
		{storedTask("running", "", workspaceReady), "Running", "10.0.0.5", "running", "True", "TaskRunning"},
		{storedTask("setting-up", ""), "Running", "10.0.0.6", "setting-up", "False", "WorkspaceInitializing"},
		{storedTask("suspended", "", workspaceReady), "Suspended", "", "suspended", "False", "TaskSuspended"},
		{storedTask("crashed", ""), v1alpha1.PhaseFailed, "", "crashed", "False", "ActorCrashed"},
		// The actor was removed outside AX.
		{storedTask("gone", ""), v1alpha1.PhaseFailed, "", "", "False", "ActorNotFound"},
		// Phases that describe AX's own work are kept, and so is their Ready reason.
		{storedTask("failed", v1alpha1.PhaseFailed, &v1alpha1.Condition{Type: "Ready", Status: "False", Reason: "ActorResumeFailed"}), v1alpha1.PhaseFailed, "", "failed", "False", "ActorResumeFailed"},
		{storedTask("not-yet", v1alpha1.PhasePending), v1alpha1.PhasePending, "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.task.GetMetadata().GetName(), func(t *testing.T) {
			r.Observe(context.Background(), []*v1alpha1.Task{tt.task})
			st := tt.task.GetStatus()
			if st.GetPhase() != tt.wantPhase || st.GetWorkerIp() != tt.wantIP || st.GetActor() != tt.wantActor {
				t.Errorf("phase=%q workerIP=%q actor=%q, want %q %q %q", st.GetPhase(), st.GetWorkerIp(), st.GetActor(), tt.wantPhase, tt.wantIP, tt.wantActor)
			}
			if tt.wantReady == "" {
				return
			}
			assertCondition(t, tt.task, "Ready", tt.wantReady, tt.wantReadyReason)
		})
	}

	t.Run("crash message", func(t *testing.T) {
		task := storedTask("crashed", "")
		r.Observe(context.Background(), []*v1alpha1.Task{task})
		for _, c := range task.GetStatus().GetConditions() {
			if c.GetType() == "Ready" && c.GetMessage() != "OOMKilled" {
				t.Errorf("Ready message = %q, want the actor's crash message", c.GetMessage())
			}
		}
	})
}

func TestObserve_ListUsesOneCallPerAtespace(t *testing.T) {
	srv := &actorsServer{actors: map[string]*ateapipb.Actor{
		"default/a": actor("default", "a", ateapipb.ActorState_ACTOR_STATE_RUNNING, "10.0.0.1"),
		"default/b": actor("default", "b", ateapipb.ActorState_ACTOR_STATE_SUSPENDED, ""),
		"team-x/c":  actor("team-x", "c", ateapipb.ActorState_ACTOR_STATE_RUNNING, "10.0.0.3"),
	}}
	r := newObserver(t, srv)

	tasks := []*v1alpha1.Task{storedTask("a", ""), storedTask("b", ""), storedTask("c", "")}
	tasks[2].Metadata.Atespace = "team-x"
	r.Observe(context.Background(), tasks)

	if srv.listCalls != 2 || srv.getCalls != 0 {
		t.Errorf("ListActors calls = %d, GetActor calls = %d, want 2 and 0", srv.listCalls, srv.getCalls)
	}
	for i, want := range []string{"Running", "Suspended", "Running"} {
		if got := tasks[i].GetStatus().GetPhase(); got != want {
			t.Errorf("task %s phase = %q, want %q", tasks[i].GetMetadata().GetName(), got, want)
		}
	}
	if ip := tasks[2].GetStatus().GetWorkerIp(); ip != "10.0.0.3" {
		t.Errorf("task c workerIP = %q, want 10.0.0.3", ip)
	}
}

func TestObserve_SubstrateErrorReportsUnknown(t *testing.T) {
	srv := &actorsServer{err: status.Error(codes.PermissionDenied, "denied")}
	r := newObserver(t, srv)

	task := storedTask("a", "")
	terminating := storedTask("b", v1alpha1.PhaseTerminating)
	r.Observe(context.Background(), []*v1alpha1.Task{task})
	r.Observe(context.Background(), []*v1alpha1.Task{terminating})

	if got := task.GetStatus().GetPhase(); got != "Unknown" {
		t.Errorf("phase = %q, want Unknown", got)
	}
	assertCondition(t, task, "Ready", "Unknown", "SubstrateUnavailable")
	if got := terminating.GetStatus().GetPhase(); got != v1alpha1.PhaseTerminating {
		t.Errorf("phase = %q, want %q to be kept", got, v1alpha1.PhaseTerminating)
	}
}
