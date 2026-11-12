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
	"slices"
	"strings"
	"testing"

	"github.com/google/ax/internal/server"
	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/internal/substrate/substratetest"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newWorkspaceTestServer(t *testing.T) *server.Server {
	t.Helper()
	sub := substratetest.NewServer(t)
	return server.NewServer(memory.NewStore(), newTestReconciler(sub))
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

func TestCreateWorkspace_RejectsUnsupportedGateway(t *testing.T) {
	srv := newWorkspaceTestServer(t)

	tests := []struct {
		name      string
		spec      *v1alpha1.WorkspaceSpec
		wantField string
	}{
		{
			name: "setup_gateway",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Gateway: &v1alpha1.GatewayRef{Name: "build-gw"},
				},
			},
			wantField: "spec.setup.gateway is not supported yet",
		},
		{
			name: "mcp_server_gateway",
			spec: &v1alpha1.WorkspaceSpec{
				Mcp: &v1alpha1.MCPConfig{
					Servers: []*v1alpha1.MCPServer{{
						Name:     "gh",
						Endpoint: "https://example.com/mcp",
						Gateway:  &v1alpha1.GatewayRef{Name: "gh-gw"},
					}},
				},
			},
			wantField: "spec.mcp.servers[0].gateway is not supported yet",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := srv.CreateWorkspace(t.Context(), &v1alpha1.CreateWorkspaceRequest{
				Workspace: &v1alpha1.Workspace{
					Metadata: &v1alpha1.ObjectMeta{Name: "gw-ws"},
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
