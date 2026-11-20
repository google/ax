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

package server_test

import (
	"testing"
	"time"

	"github.com/google/ax/internal/lifecycle"
	"github.com/google/ax/internal/server"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/internal/substrate/substratetest"
	runnerv1 "github.com/google/ax/pkg/apis/runner/v1"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestServerForkTask_FromCompletedPreservesSessionAndSeedsSnapshot(t *testing.T) {
	ctx := t.Context()
	sub := substratetest.NewServer(t)
	memStore := memory.NewStore()
	rec := newTestReconciler(sub)
	applier := server.TaskApplierForStore(memStore)
	srv := server.NewServer(memStore, rec, server.Options{TaskApplier: applier})

	if _, err := srv.CreateWorkspace(ctx, &v1alpha1.CreateWorkspaceRequest{Workspace: &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "ws-1"},
		Spec:     &v1alpha1.WorkspaceSpec{},
	}}); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if _, err := srv.CreateGateway(ctx, &v1alpha1.CreateGatewayRequest{Gateway: &v1alpha1.Gateway{
		Metadata: &v1alpha1.ObjectMeta{Name: "gw-1"},
		Spec: &v1alpha1.GatewaySpec{
			Egress: []*ateapipb.EgressRule{{
				Https: &ateapipb.HttpsRule{
					Hostnames: []string{"generativelanguage.googleapis.com"},
				},
			}},
		},
	}}); err != nil {
		t.Fatalf("CreateGateway: %v", err)
	}

	turn1, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "turn-1", Labels: map[string]string{"env": "dev", "step": "1"}},
		Spec: &v1alpha1.TaskSpec{
			Profile:   &v1alpha1.Profile{Image: "ghcr.io/example/antigravity@sha256:aaaa"},
			Workspace: &v1alpha1.WorkspaceRef{Name: "ws-1"},
			Gateway:   &v1alpha1.GatewayRef{Name: "gw-1"},
			Env: []*v1alpha1.EnvVar{
				{Name: "GEMINI_API_KEY", Value: "placeholder"},
				{Name: "MODE", Value: "turn1"},
			},
			SessionId: "hero-demo-session",
			Goal:      &v1alpha1.Goal{Text: "Add subtract(a, b)"},
			Debug:     true,
		},
	}})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Simulate Turn 1 running to COMPLETED (terminal, actor parked).
	if _, err := srv.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Name: "turn-1"}); err != nil {
		t.Fatalf("ResumeTask(turn-1): %v", err)
	}
	key1 := store.TaskKey{Atespace: "default", Name: "turn-1"}
	if _, err := applier.Apply(ctx, key1, func(current *store.TaskRow, _ time.Time) (*store.TaskRow, error) {
		next := current.Clone()
		next.Runnable = false
		next.ActiveRunID = ""
		next.Task.Status.State = v1alpha1.TaskState_TASK_STATE_COMPLETED
		next.Task.Status.Outcome = &v1alpha1.TaskOutcome{
			Result:   v1alpha1.TaskResult_TASK_RESULT_SUCCEEDED,
			ExitCode: 0,
			Output:   "added subtract",
		}
		return next, nil
	}); err != nil {
		t.Fatalf("mark turn-1 COMPLETED: %v", err)
	}

	turn2, err := srv.ForkTask(ctx, &v1alpha1.ForkTaskRequest{
		SourceTask: "turn-1",
		Name:       "turn-2",
		Labels:     map[string]string{"step": "2"},
		Overrides: &v1alpha1.ForkOverrides{
			Goal: &v1alpha1.Goal{Text: "Add multiply(a, b)"},
			Env: []*v1alpha1.EnvVar{
				{Name: "MODE", Value: "turn2"},
				{Name: "EXTRA", Value: "yes"},
			},
		},
	})
	if err != nil {
		t.Fatalf("ForkTask: %v", err)
	}

	if turn2.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_SUSPENDED {
		t.Errorf("turn-2 state = %v, want SUSPENDED", turn2.GetStatus().GetState())
	}
	if turn2.GetSpec().GetSessionId() != "hero-demo-session" {
		t.Errorf("turn-2 session_id = %q, want hero-demo-session", turn2.GetSpec().GetSessionId())
	}
	if turn2.GetSpec().GetGoal().GetText() != "Add multiply(a, b)" {
		t.Errorf("turn-2 goal = %q, want Add multiply(a, b)", turn2.GetSpec().GetGoal().GetText())
	}
	if turn2.GetStatus().GetParentTask() != "turn-1" {
		t.Errorf("turn-2 parent_task = %q, want turn-1", turn2.GetStatus().GetParentTask())
	}
	tagName := turn2.GetStatus().GetParentTag()
	if tagName == "" {
		t.Fatal("expected turn-2 parent_tag to be populated")
	}
	if turn2.GetMetadata().GetLabels()["env"] != "dev" || turn2.GetMetadata().GetLabels()["step"] != "2" {
		t.Errorf("turn-2 labels = %v, want env=dev step=2", turn2.GetMetadata().GetLabels())
	}
	envMap := make(map[string]string)
	for _, e := range turn2.GetSpec().GetEnv() {
		envMap[e.GetName()] = e.GetValue()
	}
	if envMap["GEMINI_API_KEY"] != "placeholder" || envMap["MODE"] != "turn2" || envMap["EXTRA"] != "yes" {
		t.Errorf("merged env = %v, want GEMINI_API_KEY=placeholder MODE=turn2 EXTRA=yes", envMap)
	}

	// Verify Substrate actor was seeded from the fork snapshot tag and shares
	// the parent's ActorTemplate and gateway egress rules.
	actor2, ok := sub.Actor("default", "turn-2")
	if !ok {
		t.Fatal("expected turn-2 actor on Substrate")
	}
	if actor2.GetSourceTag().GetName() != tagName {
		t.Errorf("turn-2 actor source_tag = %q, want %q", actor2.GetSourceTag().GetName(), tagName)
	}
	if actor2.GetActorTemplate().GetName() != turn1.GetStatus().GetAteTemplate().GetName() {
		t.Errorf("turn-2 template = %q, want %q", actor2.GetActorTemplate().GetName(), turn1.GetStatus().GetAteTemplate().GetName())
	}
	if _, ok := sub.EgressPolicy("default", "turn-2"); !ok {
		t.Error("expected turn-2 actor to have an egress policy")
	}

	// Original turn-1 must remain COMPLETED and immutable.
	got1, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "turn-1"})
	if err != nil {
		t.Fatalf("GetTask(turn-1): %v", err)
	}
	if got1.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_COMPLETED {
		t.Errorf("turn-1 state = %v, want COMPLETED", got1.GetStatus().GetState())
	}

	// Resuming turn-2 lets its pull runner claim a fresh r-1 run with Turn 2's
	// goal and inherited session_id.
	if _, err := srv.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Name: "turn-2"}); err != nil {
		t.Fatalf("ResumeTask(turn-2): %v", err)
	}
	row2, err := applier.GetTaskRow(ctx, store.TaskKey{Atespace: "default", Name: "turn-2"})
	if err != nil {
		t.Fatalf("GetTaskRow(turn-2): %v", err)
	}
	_, claimResp, err := lifecycle.Claim(row2, &runnerv1.ClaimAssignmentRequest{Nonce: "n-1"}, nil, "", nil, time.Now())
	if err != nil {
		t.Fatalf("lifecycle.Claim(turn-2): %v", err)
	}
	if claimResp.GetMode() != runnerv1.AssignmentMode_ASSIGNMENT_MODE_RUN {
		t.Errorf("claim mode = %v, want RUN", claimResp.GetMode())
	}
	if claimResp.GetGoal().GetText() != "Add multiply(a, b)" {
		t.Errorf("claim goal = %q, want Add multiply(a, b)", claimResp.GetGoal().GetText())
	}
	if claimResp.GetTask().GetSpec().GetSessionId() != "hero-demo-session" {
		t.Errorf("claim session_id = %q, want hero-demo-session", claimResp.GetTask().GetSpec().GetSessionId())
	}

	// Deleting turn-2 removes its fork tag cleanly.
	if _, err := srv.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Name: "turn-2"}); err != nil {
		t.Fatalf("DeleteTask(turn-2): %v", err)
	}
	if _, ok := sub.Tag("default", tagName); ok {
		t.Errorf("expected fork tag %q to be removed on DeleteTask", tagName)
	}
}

func TestServerForkTask_OverridesAndValidation(t *testing.T) {
	ctx := t.Context()
	sub := substratetest.NewServer(t)
	memStore := memory.NewStore()
	srv := server.NewServer(memStore, newTestReconciler(sub))

	// Create a suspended source task.
	src, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "base-task"},
		Spec: &v1alpha1.TaskSpec{
			Profile: &v1alpha1.Profile{Image: "alpine"},
			Goal:    &v1alpha1.Goal{Text: "initial"},
		},
	}})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	cfg, err := structpb.NewStruct(map[string]any{"model": "gemini-pro"})
	if err != nil {
		t.Fatalf("NewStruct: %v", err)
	}
	forked, err := srv.ForkTask(ctx, &v1alpha1.ForkTaskRequest{
		SourceTask: "base-task",
		Name:       "branch-task",
		Overrides: &v1alpha1.ForkOverrides{
			Commands:  []string{"sh", "-c", "echo hi"},
			Config:    cfg,
			SessionId: "custom-session",
		},
	})
	if err != nil {
		t.Fatalf("ForkTask(suspended): %v", err)
	}
	if forked.GetSpec().GetSessionId() != "custom-session" {
		t.Errorf("session_id = %q, want custom-session", forked.GetSpec().GetSessionId())
	}
	if got := forked.GetSpec().GetCommands(); len(got) != 3 || got[0] != "sh" {
		t.Errorf("commands = %v, want [sh -c 'echo hi']", got)
	}
	if forked.GetSpec().GetConfig().AsMap()["model"] != "gemini-pro" {
		t.Errorf("config = %v, want model=gemini-pro", forked.GetSpec().GetConfig().AsMap())
	}
	if forked.GetSpec().GetGoal().GetText() != "initial" {
		t.Errorf("goal = %q, want inherited 'initial'", forked.GetSpec().GetGoal().GetText())
	}
	if forked.GetMetadata().GetUid() == src.GetMetadata().GetUid() {
		t.Error("forked task must receive a distinct metadata.uid")
	}

	// Duplicate target name fails with AlreadyExists.
	if _, err := srv.ForkTask(ctx, &v1alpha1.ForkTaskRequest{SourceTask: "base-task", Name: "branch-task"}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate fork code = %v, want AlreadyExists (err=%v)", status.Code(err), err)
	}

	// Missing source fails with NotFound.
	if _, err := srv.ForkTask(ctx, &v1alpha1.ForkTaskRequest{SourceTask: "missing", Name: "fork-x"}); status.Code(err) != codes.NotFound {
		t.Errorf("missing source code = %v, want NotFound (err=%v)", status.Code(err), err)
	}

	// Invalid requests fail with InvalidArgument.
	for _, tc := range []struct {
		name string
		req  *v1alpha1.ForkTaskRequest
	}{
		{name: "nil", req: nil},
		{name: "missing source", req: &v1alpha1.ForkTaskRequest{Name: "a"}},
		{name: "missing name", req: &v1alpha1.ForkTaskRequest{SourceTask: "base-task"}},
		{name: "same name", req: &v1alpha1.ForkTaskRequest{SourceTask: "base-task", Name: "base-task"}},
		{name: "bad name", req: &v1alpha1.ForkTaskRequest{SourceTask: "base-task", Name: "INVALID_NAME"}},
		{name: "reserved setup prefix", req: &v1alpha1.ForkTaskRequest{SourceTask: "base-task", Name: "ax-ws-setup-foo"}},
	} {
		if _, err := srv.ForkTask(ctx, tc.req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: code = %v, want InvalidArgument (err=%v)", tc.name, status.Code(err), err)
		}
	}

	// Running (WORKING) source task cannot be forked.
	if _, err := srv.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Name: "base-task"}); err != nil {
		t.Fatalf("ResumeTask: %v", err)
	}
	if _, err := srv.ForkTask(ctx, &v1alpha1.ForkTaskRequest{SourceTask: "base-task", Name: "fork-working"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("forking WORKING task code = %v, want FailedPrecondition (err=%v)", status.Code(err), err)
	}
}
