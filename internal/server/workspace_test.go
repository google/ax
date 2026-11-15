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
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/ax/internal/controller"
	"github.com/google/ax/internal/server"
	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/internal/substrate/substratetest"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type testSetupCoordinator struct {
	calls atomic.Int32
	err   error
}

func (c *testSetupCoordinator) RegisterSetup(context.Context, string, string, string, *v1alpha1.Workspace) error {
	return nil
}

func (c *testSetupCoordinator) AwaitSetup(context.Context, string, string) error {
	c.calls.Add(1)
	return c.err
}

func (c *testSetupCoordinator) DeleteSetup(context.Context, string, string) error {
	return nil
}

func newWorkspaceTestServer(t *testing.T) *server.Server {
	t.Helper()
	sub := substratetest.NewServer(t)
	return server.NewServer(memory.NewStore(), newTestReconciler(sub))
}

func newPrewarmWorkspaceTestServer(t *testing.T, coord controller.SetupCoordinator) (*server.Server, *substratetest.Server) {
	t.Helper()
	sub := substratetest.NewServer(t)
	wsMgr := controller.NewWorkspaceManager(sub.Client(), controller.WorkspaceManagerOptions{
		SetupCoordinator: coord,
	})
	srv := server.NewServer(memory.NewStore(), newTestReconciler(sub), server.Options{
		WorkspaceManager: wsMgr,
	})
	return srv, sub
}

func TestCreateWorkspace_Immutability(t *testing.T) {
	srv := newWorkspaceTestServer(t)
	ctx := t.Context()

	wsSpec := &v1alpha1.WorkspaceSpec{
		Setup: &v1alpha1.WorkspaceSetup{
			Commands: []string{"echo setup"},
		},
	}
	created, err := srv.CreateWorkspace(ctx, &v1alpha1.CreateWorkspaceRequest{
		Workspace: &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: "imm-ws"},
			Spec:     wsSpec,
		},
	})
	if err != nil {
		t.Fatalf("CreateWorkspace() failed: %v", err)
	}

	again, err := srv.CreateWorkspace(ctx, &v1alpha1.CreateWorkspaceRequest{
		Workspace: &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: "imm-ws"},
			Spec:     wsSpec,
		},
	})
	if err != nil {
		t.Fatalf("CreateWorkspace(idempotent) failed: %v", err)
	}
	if !again.GetMetadata().GetCreateTime().AsTime().Equal(created.GetMetadata().GetCreateTime().AsTime()) {
		t.Errorf("CreateTime = %v, want %v", again.GetMetadata().GetCreateTime(), created.GetMetadata().GetCreateTime())
	}

	_, err = srv.CreateWorkspace(ctx, &v1alpha1.CreateWorkspaceRequest{
		Workspace: &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: "imm-ws"},
			Spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{Commands: []string{"echo mutated"}},
			},
		},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("CreateWorkspace(mutated) = %v, want FailedPrecondition", err)
	}
}

func TestCreateWorkspace_RejectsUnimplementedFields(t *testing.T) {
	srv := newWorkspaceTestServer(t)

	tests := []struct {
		name      string
		spec      *v1alpha1.WorkspaceSpec
		wantField string
	}{
		{
			name: "profile",
			spec: &v1alpha1.WorkspaceSpec{
				Profile: &v1alpha1.Profile{
					Resources: &v1alpha1.ResourcesSpec{
						Limits: &v1alpha1.ResourceList{Cpu: "1", Memory: "512Mi"},
					},
				},
			},
			wantField: "spec.profile is not supported yet",
		},
		{
			name: "setup_gateway",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Gateway: &v1alpha1.GatewayRef{Name: "build-gw"},
				},
			},
			wantField: "spec.setup.gateway is not supported yet",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := srv.CreateWorkspace(t.Context(), &v1alpha1.CreateWorkspaceRequest{
				Workspace: &v1alpha1.Workspace{
					Metadata: &v1alpha1.ObjectMeta{Name: "unimpl-ws"},
					Spec:     tc.spec,
				},
			})
			if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), tc.wantField) {
				t.Fatalf("CreateWorkspace() = %v, want Unimplemented containing %q", err, tc.wantField)
			}
		})
	}
}

func TestListWorkspaces_Pagination(t *testing.T) {
	srv := newWorkspaceTestServer(t)
	ctx := t.Context()

	for _, name := range []string{"ws-a", "ws-b", "ws-c"} {
		if _, err := srv.CreateWorkspace(ctx, &v1alpha1.CreateWorkspaceRequest{
			Workspace: &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: name}},
		}); err != nil {
			t.Fatalf("CreateWorkspace(%s) failed: %v", name, err)
		}
	}

	tests := []struct {
		name   string
		limit  int64
		offset int64
		want   []string
	}{
		{name: "first_page", limit: 2, offset: 0, want: []string{"ws-a", "ws-b"}},
		{name: "middle_page", limit: 1, offset: 1, want: []string{"ws-b"}},
		{name: "out_of_range", limit: 10, offset: 99, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := srv.ListWorkspaces(t.Context(), &v1alpha1.ListWorkspacesRequest{
				Atespace: "default",
				Limit:    tc.limit,
				Offset:   tc.offset,
			})
			if err != nil {
				t.Fatalf("ListWorkspaces() failed: %v", err)
			}
			var got []string
			for _, ws := range resp.GetWorkspaces() {
				got = append(got, ws.GetMetadata().GetName())
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ListWorkspaces(limit=%d, offset=%d) = %v, want %v", tc.limit, tc.offset, got, tc.want)
			}
		})
	}
}

func TestWorkspacePrewarmLifecycle(t *testing.T) {
	coord := &testSetupCoordinator{}
	srv, sub := newPrewarmWorkspaceTestServer(t, coord)
	ctx := t.Context()

	wsSpec := &v1alpha1.WorkspaceSpec{
		Profile: &v1alpha1.Profile{
			Image: "ghcr.io/org/agent@sha256:abc",
			Resources: &v1alpha1.ResourcesSpec{
				Limits: &v1alpha1.ResourceList{Cpu: "2", Memory: "4Gi"},
			},
		},
		Setup: &v1alpha1.WorkspaceSetup{
			Commands: []string{"npm ci"},
			Gateway:  &v1alpha1.GatewayRef{Name: "setup-gw"},
		},
	}

	// Missing setup gateway is rejected at CreateWorkspace time.
	_, err := srv.CreateWorkspace(ctx, &v1alpha1.CreateWorkspaceRequest{
		Workspace: &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: "prewarm-ws"},
			Spec:     wsSpec,
		},
	})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), `"setup-gw" not found`) {
		t.Fatalf("CreateWorkspace(missing gateway) = %v, want FailedPrecondition", err)
	}

	if _, err := srv.CreateGateway(ctx, &v1alpha1.CreateGatewayRequest{
		Gateway: &v1alpha1.Gateway{
			Metadata: &v1alpha1.ObjectMeta{Name: "setup-gw"},
			Spec: &v1alpha1.GatewaySpec{
				Egress: []*ateapipb.EgressRule{{
					Https: &ateapipb.HttpsRule{Hostnames: []string{"registry.npmjs.org"}},
				}},
			},
		},
	}); err != nil {
		t.Fatalf("CreateGateway: %v", err)
	}

	created, err := srv.CreateWorkspace(ctx, &v1alpha1.CreateWorkspaceRequest{
		Workspace: &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: "prewarm-ws"},
			Spec:     wsSpec,
		},
	})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if created.GetStatus().GetPhase() != v1alpha1.WorkspacePhaseReady {
		t.Fatalf("Workspace status = %+v, want Ready", created.GetStatus())
	}
	if coord.calls.Load() != 1 {
		t.Fatalf("AwaitSetup calls = %d, want 1", coord.calls.Load())
	}

	tags, err := sub.Client().ListTags(ctx, "default")
	if err != nil || len(tags) != 1 {
		t.Fatalf("ListTags = (%+v, %v), want 1 prewarmed tag", tags, err)
	}

	// Idempotent re-apply returns Ready without re-running setup.
	again, err := srv.CreateWorkspace(ctx, &v1alpha1.CreateWorkspaceRequest{
		Workspace: &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: "prewarm-ws"},
			Spec:     wsSpec,
		},
	})
	if err != nil || again.GetStatus().GetPhase() != v1alpha1.WorkspacePhaseReady || coord.calls.Load() != 1 {
		t.Fatalf("re-apply = (%+v, %v), calls=%d", again, err, coord.calls.Load())
	}

	// DeleteWorkspace sweeps Substrate tags as well as the store record.
	if _, err := srv.DeleteWorkspace(ctx, &v1alpha1.DeleteWorkspaceRequest{Name: "prewarm-ws"}); err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}
	if tags, _ := sub.Client().ListTags(ctx, "default"); len(tags) != 0 {
		t.Errorf("tags remaining after DeleteWorkspace: %+v", tags)
	}
	if _, err := srv.GetWorkspace(ctx, &v1alpha1.GetWorkspaceRequest{Name: "prewarm-ws"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetWorkspace after DeleteWorkspace = %v, want NotFound", err)
	}
}

func TestWorkspacePrewarm_FailureBlocksCreateTask(t *testing.T) {
	coord := &testSetupCoordinator{err: errors.New("git clone exit status 128")}
	srv, _ := newPrewarmWorkspaceTestServer(t, coord)
	ctx := t.Context()

	_, err := srv.CreateWorkspace(ctx, &v1alpha1.CreateWorkspaceRequest{
		Workspace: &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: "broken-ws"},
			Spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{Commands: []string{"false"}},
			},
		},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("CreateWorkspace(setup failure) = %v, want FailedPrecondition", err)
	}

	stored, err := srv.GetWorkspace(ctx, &v1alpha1.GetWorkspaceRequest{Name: "broken-ws"})
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if stored.GetStatus().GetPhase() != v1alpha1.WorkspacePhaseFailed || !strings.Contains(stored.GetStatus().GetLastSetupError(), "exit status 128") {
		t.Fatalf("stored status = %+v, want Failed with error detail", stored.GetStatus())
	}

	// CreateTask referencing a Failed workspace is rejected immediately.
	_, err = srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "task-on-broken-ws"},
			Spec: &v1alpha1.TaskSpec{
				Profile:   &v1alpha1.Profile{Image: "ghcr.io/org/agent@sha256:abc"},
				Workspace: &v1alpha1.WorkspaceRef{Name: "broken-ws"},
			},
		},
	})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "failed setup") {
		t.Fatalf("CreateTask(broken workspace) = %v, want FailedPrecondition mentioning failed setup", err)
	}
}

func TestWorkspacePrewarm_TransientRetryHeals(t *testing.T) {
	coord := &testSetupCoordinator{}
	srv, sub := newPrewarmWorkspaceTestServer(t, coord)
	ctx := t.Context()

	sub.SetErrors(substratetest.Errors{
		CreateAtespace: status.Error(codes.DeadlineExceeded, "substrate busy"),
	})
	wsReq := &v1alpha1.CreateWorkspaceRequest{
		Workspace: &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: "retry-ws"},
			Spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{Commands: []string{"true"}},
			},
		},
	}

	if _, err := srv.CreateWorkspace(ctx, wsReq); status.Code(err) != codes.Unavailable {
		t.Fatalf("first CreateWorkspace = %v, want Unavailable", err)
	}
	pending, err := srv.GetWorkspace(ctx, &v1alpha1.GetWorkspaceRequest{Name: "retry-ws"})
	if err != nil || pending.GetStatus().GetPhase() != v1alpha1.WorkspacePhasePreparing {
		t.Fatalf("GetWorkspace after transient error = (%+v, %v), want Preparing", pending, err)
	}

	// CreateTask referencing a Preparing workspace is rejected until setup finishes.
	_, err = srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "task-on-preparing-ws"},
			Spec: &v1alpha1.TaskSpec{
				Profile:   &v1alpha1.Profile{Image: "ghcr.io/org/agent@sha256:abc"},
				Workspace: &v1alpha1.WorkspaceRef{Name: "retry-ws"},
			},
		},
	})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "still preparing") {
		t.Fatalf("CreateTask(preparing workspace) = %v, want FailedPrecondition mentioning still preparing", err)
	}

	// Clear transient Substrate error; re-applying the same manifest finishes Prepare.
	sub.SetErrors(substratetest.Errors{})
	ready, err := srv.CreateWorkspace(ctx, &v1alpha1.CreateWorkspaceRequest{
		Workspace: &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: "retry-ws"},
			Spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{Commands: []string{"true"}},
			},
		},
	})
	if err != nil || ready.GetStatus().GetPhase() != v1alpha1.WorkspacePhaseReady {
		t.Fatalf("retry CreateWorkspace = (%+v, %v), want Ready", ready, err)
	}
}
