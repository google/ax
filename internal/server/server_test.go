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
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/ax/internal/controller"
	"github.com/google/ax/internal/server"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/internal/substrate/substratetest"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestServerHealthzHTTP(t *testing.T) {
	memStore := memory.NewStore()
	srv := server.NewServer(memStore, newTestTaskManager(substratetest.NewServer(t)))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK from /healthz, got %d", rec.Code)
	}
	if rec.Body.String() != "ok\n" {
		t.Fatalf("expected 'ok\\n' from /healthz, got %q", rec.Body.String())
	}

	// Verify non-healthz HTTP returns 404
	req404 := httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil)
	rec404 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec404, req404)
	if rec404.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found for /api/v1/tasks, got %d", rec404.Code)
	}
}

func TestServerGRPC(t *testing.T) {
	memStore := memory.NewStore()
	srv := server.NewServer(memStore, newTestTaskManager(substratetest.NewServer(t)))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()

	httpServer := &http.Server{
		Handler: srv.Handler(),
	}
	httpServer.Protocols = new(http.Protocols)
	httpServer.Protocols.SetHTTP1(true)
	httpServer.Protocols.SetUnencryptedHTTP2(true)

	go func() {
		_ = httpServer.Serve(ln)
	}()
	defer httpServer.Close()

	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to dial gRPC: %v", err)
	}
	defer conn.Close()

	client := v1alpha1.NewAXClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. Create one resource of each kind through the typed RPCs. Metadata is left
	// partially empty to exercise server-side defaulting.
	if _, err := client.CreateWorkspace(ctx, &v1alpha1.CreateWorkspaceRequest{Workspace: &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "grpc-ws"},
		Spec:     &v1alpha1.WorkspaceSpec{},
	}}); err != nil {
		t.Fatalf("CreateWorkspace failed: %v", err)
	}
	if _, err := client.UpdateModel(ctx, &v1alpha1.UpdateModelRequest{Model: &v1alpha1.Model{
		Metadata: &v1alpha1.ObjectMeta{Name: "grpc-model"},
		Spec:     &v1alpha1.ModelSpec{Provider: "google", Model: "gemini-3.8-flash"},
	}}); err != nil {
		t.Fatalf("UpdateModel failed: %v", err)
	}
	if _, err := client.CreateGateway(ctx, &v1alpha1.CreateGatewayRequest{Gateway: &v1alpha1.Gateway{
		Metadata: &v1alpha1.ObjectMeta{Name: "grpc-gw"},
		Spec:     &v1alpha1.GatewaySpec{},
	}}); err != nil {
		t.Fatalf("CreateGateway failed: %v", err)
	}
	if _, err := client.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "grpc-task"},
		Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
	}}); err != nil {
		t.Fatalf("CreateTask failed: %v", err)
	}

	// 2. Defaulting applies to every kind: atespace and creation timestamp are filled in.
	ws, err := client.GetWorkspace(ctx, &v1alpha1.GetWorkspaceRequest{Atespace: "default", Name: "grpc-ws"})
	if err != nil {
		t.Fatalf("GetWorkspace failed: %v", err)
	}
	if ws.GetMetadata().GetAtespace() != "default" || ws.GetMetadata().GetCreateTime() == nil {
		t.Errorf("expected workspace metadata to be defaulted, got %v", ws.GetMetadata())
	}

	// 3. GetTask & ListTasks
	task, err := client.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: "default", Name: "grpc-task"})
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if task.Metadata.Name != "grpc-task" {
		t.Errorf("expected name 'grpc-task', got %s", task.Metadata.Name)
	}
	if task.Metadata.CreateTime == nil {
		t.Errorf("expected creation timestamp to be populated on applied task")
	}
	if task.Metadata.Uid == "" {
		t.Errorf("expected server-assigned uid to be populated on applied task")
	}
	if task.Spec.SessionId != task.Metadata.Uid {
		t.Errorf("expected session_id to default to metadata.uid (%q), got %q", task.Metadata.Uid, task.Spec.SessionId)
	}
	if task.Status.Attempt != 1 {
		t.Errorf("expected attempt = 1, got %d", task.Status.Attempt)
	}

	listTasksResp, err := client.ListTasks(ctx, &v1alpha1.ListTasksRequest{Atespace: "default"})
	if err != nil {
		t.Fatalf("ListTasks failed: %v", err)
	}
	if len(listTasksResp.Tasks) != 1 {
		t.Fatalf("expected 1 task in list, got %d", len(listTasksResp.Tasks))
	}
	if listTasksResp.Tasks[0].Metadata.CreateTime == nil {
		t.Errorf("expected creation timestamp on listed task")
	}

	// Test that Task is immutable
	task.Spec.Profile = &v1alpha1.Profile{Image: "ghcr.io/test/updated-image"}
	_, err = client.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: task})
	if err == nil {
		t.Fatalf("expected CreateTask to fail on existing task because tasks are immutable")
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("expected FailedPrecondition code, got %v", status.Code(err))
	}

	// 4. Suspend & Resume Task
	suspTask, err := client.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{Atespace: "default", Name: "grpc-task"})
	if err != nil {
		t.Fatalf("SuspendTask failed: %v", err)
	}
	if suspTask.Status.State != v1alpha1.TaskState_TASK_STATE_SUSPENDED {
		t.Errorf("expected task state to be SUSPENDED, got %v", suspTask.Status.State)
	}

	resTask, err := client.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Atespace: "default", Name: "grpc-task"})
	if err != nil {
		t.Fatalf("ResumeTask failed: %v", err)
	}
	if resTask.Status.State != v1alpha1.TaskState_TASK_STATE_RUNNING {
		t.Errorf("expected task state to be RUNNING, got %v", resTask.Status.State)
	}

	// 5. Workspaces
	ws, err = client.GetWorkspace(ctx, &v1alpha1.GetWorkspaceRequest{Atespace: "default", Name: "grpc-ws"})
	if err != nil {
		t.Fatalf("GetWorkspace failed: %v", err)
	}
	if ws.Metadata.Name != "grpc-ws" {
		t.Errorf("expected workspace 'grpc-ws', got %s", ws.Metadata.Name)
	}

	listWorkspacesResp, err := client.ListWorkspaces(ctx, &v1alpha1.ListWorkspacesRequest{Atespace: "default"})
	if err != nil {
		t.Fatalf("ListWorkspaces failed: %v", err)
	}
	if len(listWorkspacesResp.Workspaces) != 1 {
		t.Fatalf("expected 1 workspace in list, got %d", len(listWorkspacesResp.Workspaces))
	}

	// 6. Gateways
	gw, err := client.GetGateway(ctx, &v1alpha1.GetGatewayRequest{Atespace: "default", Name: "grpc-gw"})
	if err != nil {
		t.Fatalf("GetGateway failed: %v", err)
	}
	if gw.Metadata.Name != "grpc-gw" {
		t.Errorf("expected gateway 'grpc-gw', got %s", gw.Metadata.Name)
	}

	listGatewaysResp, err := client.ListGateways(ctx, &v1alpha1.ListGatewaysRequest{Atespace: "default"})
	if err != nil {
		t.Fatalf("ListGateways failed: %v", err)
	}
	if len(listGatewaysResp.Gateways) != 1 {
		t.Fatalf("expected 1 gateway in list, got %d", len(listGatewaysResp.Gateways))
	}

	// 7. Models
	model, err := client.GetModel(ctx, &v1alpha1.GetModelRequest{Atespace: "default", Name: "grpc-model"})
	if err != nil {
		t.Fatalf("GetModel failed: %v", err)
	}
	if model.Metadata.Name != "grpc-model" {
		t.Errorf("expected model 'grpc-model', got %s", model.Metadata.Name)
	}

	listModelsResp, err := client.ListModels(ctx, &v1alpha1.ListModelsRequest{Atespace: "default"})
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}
	if len(listModelsResp.Models) != 1 {
		t.Fatalf("expected 1 model in list, got %d", len(listModelsResp.Models))
	}

	// 8. Delete operations
	if _, err := client.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Atespace: "default", Name: "grpc-task"}); err != nil {
		t.Fatalf("DeleteTask failed: %v", err)
	}
	if _, err := client.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Atespace: "default", Name: "no-such-task"}); status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound deleting a missing task, got %v", err)
	}
	if _, err := client.DeleteWorkspace(ctx, &v1alpha1.DeleteWorkspaceRequest{Atespace: "default", Name: "grpc-ws"}); err != nil {
		t.Fatalf("DeleteWorkspace failed: %v", err)
	}
	if _, err := client.DeleteWorkspace(ctx, &v1alpha1.DeleteWorkspaceRequest{Atespace: "default", Name: "no-such-workspace"}); status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound deleting a missing workspace, got %v", err)
	}
	if _, err := client.DeleteGateway(ctx, &v1alpha1.DeleteGatewayRequest{Atespace: "default", Name: "grpc-gw"}); err != nil {
		t.Fatalf("DeleteGateway failed: %v", err)
	}
	if _, err := client.DeleteGateway(ctx, &v1alpha1.DeleteGatewayRequest{Atespace: "default", Name: "no-such-gateway"}); status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound deleting a missing gateway, got %v", err)
	}
	if _, err := client.DeleteModel(ctx, &v1alpha1.DeleteModelRequest{Atespace: "default", Name: "grpc-model"}); err != nil {
		t.Fatalf("DeleteModel failed: %v", err)
	}
	if _, err := client.DeleteModel(ctx, &v1alpha1.DeleteModelRequest{Atespace: "default", Name: "no-such-model"}); status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound deleting a missing model, got %v", err)
	}

	// Check 404 after delete
	_, err = client.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: "default", Name: "grpc-task"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound code, got %v", err)
	}
}

// TestServer_ListPagination pages through every kind with page tokens, then
// checks the page size and page token rules, which the kinds share, on models.
func TestServer_ListPagination(t *testing.T) {
	st := memory.NewStore()
	srv := server.NewServer(st, newTestTaskManager(substratetest.NewServer(t)))
	ctx := context.Background()

	for _, name := range []string{"a", "b", "c", "d"} {
		meta := &v1alpha1.ObjectMeta{Name: name, Atespace: "default"}
		if err := st.SaveTask(ctx, &v1alpha1.Task{Metadata: meta}); err != nil {
			t.Fatal(err)
		}
		if err := st.SaveWorkspace(ctx, &v1alpha1.Workspace{Metadata: meta}); err != nil {
			t.Fatal(err)
		}
		if err := st.SaveGateway(ctx, &v1alpha1.Gateway{Metadata: meta}); err != nil {
			t.Fatal(err)
		}
		if err := st.SaveModel(ctx, &v1alpha1.Model{Metadata: meta}); err != nil {
			t.Fatal(err)
		}
	}
	// Each list returns the names on one page of two, and the next page token.
	lists := []struct {
		kind string
		list func(pageToken string) ([]string, string, error)
	}{
		{kind: "tasks", list: func(pageToken string) ([]string, string, error) {
			resp, err := srv.ListTasks(ctx, &v1alpha1.ListTasksRequest{Atespace: "default", PageSize: 2, PageToken: pageToken})
			return resourceNames(resp.GetTasks()), resp.GetNextPageToken(), err
		}},
		{kind: "workspaces", list: func(pageToken string) ([]string, string, error) {
			resp, err := srv.ListWorkspaces(ctx, &v1alpha1.ListWorkspacesRequest{Atespace: "default", PageSize: 2, PageToken: pageToken})
			return resourceNames(resp.GetWorkspaces()), resp.GetNextPageToken(), err
		}},
		{kind: "gateways", list: func(pageToken string) ([]string, string, error) {
			resp, err := srv.ListGateways(ctx, &v1alpha1.ListGatewaysRequest{Atespace: "default", PageSize: 2, PageToken: pageToken})
			return resourceNames(resp.GetGateways()), resp.GetNextPageToken(), err
		}},
		{kind: "models", list: func(pageToken string) ([]string, string, error) {
			resp, err := srv.ListModels(ctx, &v1alpha1.ListModelsRequest{Atespace: "default", PageSize: 2, PageToken: pageToken})
			return resourceNames(resp.GetModels()), resp.GetNextPageToken(), err
		}},
	}
	for _, tc := range lists {
		t.Run(tc.kind, func(t *testing.T) {
			first, pageToken, err := tc.list("")
			if err != nil || !slices.Equal(first, []string{"a", "b"}) || pageToken == "" {
				t.Fatalf("first page = (%v, %q, %v), want [a b] and a next page token", first, pageToken, err)
			}
			last, pageToken, err := tc.list(pageToken)
			if err != nil || !slices.Equal(last, []string{"c", "d"}) || pageToken != "" {
				t.Fatalf("last page = (%v, %q, %v), want [c d] and no next page token", last, pageToken, err)
			}
		})
	}

	// One model more than the largest page.
	for i := range 1001 {
		if err := st.SaveModel(ctx, &v1alpha1.Model{Metadata: &v1alpha1.ObjectMeta{Name: fmt.Sprintf("m%04d", i), Atespace: "big"}}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := srv.ListModels(ctx, &v1alpha1.ListModelsRequest{Atespace: "big"})
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	tests := []struct {
		name      string
		atespace  string
		pageSize  int32
		pageToken string
		want      int
		wantCode  codes.Code
	}{
		{name: "default page size", atespace: "big", want: 50},
		{name: "page size above the maximum", atespace: "big", pageSize: 5000, want: 1000},
		{name: "negative page size", atespace: "big", pageSize: -1, wantCode: codes.InvalidArgument},
		{name: "malformed page token", pageToken: "not a token", wantCode: codes.InvalidArgument},
		{name: "page token of another atespace", atespace: "default", pageToken: first.GetNextPageToken(), wantCode: codes.InvalidArgument},
		{name: "page token with a negative offset", atespace: "big", pageToken: base64.RawURLEncoding.EncodeToString([]byte(`{"atespace":"big","offset":-1}`)), wantCode: codes.InvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := srv.ListModels(ctx, &v1alpha1.ListModelsRequest{Atespace: tc.atespace, PageSize: tc.pageSize, PageToken: tc.pageToken})
			if status.Code(err) != tc.wantCode || len(resp.GetModels()) != tc.want {
				t.Fatalf("ListModels = (%d models, %v), want (%d models, %v)", len(resp.GetModels()), err, tc.want, tc.wantCode)
			}
		})
	}
}

// resourceNames returns the names of resources, in order.
func resourceNames[T interface{ GetMetadata() *v1alpha1.ObjectMeta }](resources []T) []string {
	var names []string
	for _, r := range resources {
		names = append(names, r.GetMetadata().GetName())
	}
	return names
}

// Names and atespaces become Substrate resource names, which must be RFC 1123
// labels. The server rejects them up front instead of failing during Substrate
// actor creation.
func TestCreate_RejectsInvalidNames(t *testing.T) {
	srv := server.NewServer(memory.NewStore(), newTestTaskManager(substratetest.NewServer(t)))
	ctx := context.Background()

	for _, meta := range []*v1alpha1.ObjectMeta{
		{Name: "Task-With-Caps"},
		{Name: "under_score"},
		{Name: ""},
		{Name: "ok", Atespace: "Not-Lowercase"},
	} {
		if _, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{Metadata: meta}}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateTask(%v): got %v, want InvalidArgument", meta, err)
		}
		if _, err := srv.CreateWorkspace(ctx, &v1alpha1.CreateWorkspaceRequest{Workspace: &v1alpha1.Workspace{Metadata: meta}}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateWorkspace(%v): got %v, want InvalidArgument", meta, err)
		}
		if _, err := srv.CreateGateway(ctx, &v1alpha1.CreateGatewayRequest{Gateway: &v1alpha1.Gateway{Metadata: meta}}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateGateway(%v): got %v, want InvalidArgument", meta, err)
		}
		if _, err := srv.UpdateModel(ctx, &v1alpha1.UpdateModelRequest{Model: &v1alpha1.Model{Metadata: meta}}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("UpdateModel(%v): got %v, want InvalidArgument", meta, err)
		}
	}

	// Nothing invalid was persisted.
	if resp, err := srv.ListTasks(ctx, &v1alpha1.ListTasksRequest{}); err != nil || len(resp.GetTasks()) != 0 {
		t.Errorf("ListTasks after rejected applies = %v, %v; want empty", resp.GetTasks(), err)
	}

	// Valid names still go through, with and without an explicit atespace.
	good := &v1alpha1.ObjectMeta{Name: "task-with-caps", Atespace: "team-a"}
	if _, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{Metadata: good, Spec: &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}}}}); err != nil {
		t.Errorf("CreateTask(%v): %v", good, err)
	}
	if _, err := srv.CreateWorkspace(ctx, &v1alpha1.CreateWorkspaceRequest{Workspace: &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "ws-1"}}}); err != nil {
		t.Errorf("CreateWorkspace: %v", err)
	}
	if _, err := srv.CreateGateway(ctx, &v1alpha1.CreateGatewayRequest{Gateway: &v1alpha1.Gateway{Metadata: &v1alpha1.ObjectMeta{Name: "gw-1"}}}); err != nil {
		t.Errorf("CreateGateway: %v", err)
	}
	if _, err := srv.UpdateModel(ctx, &v1alpha1.UpdateModelRequest{Model: &v1alpha1.Model{Metadata: &v1alpha1.ObjectMeta{Name: "gemini"}}}); err != nil {
		t.Errorf("UpdateModel: %v", err)
	}
}

func TestCreateTask_ValidatesWorkspaceBinding(t *testing.T) {
	srv := server.NewServer(memory.NewStore(), newTestTaskManager(substratetest.NewServer(t)))
	ctx := context.Background()

	_, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "bad"},
		Spec: &v1alpha1.TaskSpec{
			Workspace: &v1alpha1.WorkspaceRef{Path: "/w"},
		},
	}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for a nameless workspace binding, got %v", err)
	}

	if _, err := srv.CreateWorkspace(ctx, &v1alpha1.CreateWorkspaceRequest{Workspace: &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "a"},
	}}); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	_, err = srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "good"},
		Spec: &v1alpha1.TaskSpec{
			Profile:   &v1alpha1.Profile{Image: "alpine"},
			Workspace: &v1alpha1.WorkspaceRef{Name: "a"},
		},
	}})
	if err != nil {
		t.Fatalf("expected a valid workspace binding to be accepted, got %v", err)
	}
}

type failGetWorkspaceStore struct {
	store.Store
	err error
}

func (f *failGetWorkspaceStore) GetWorkspace(context.Context, string, string) (*v1alpha1.Workspace, error) {
	return nil, f.err
}

// A task is immutable, so CreateTask must not save or provision a task whose
// Workspace does not exist yet: it would run with an empty workspace forever.
func TestCreateTask_RequiresBoundWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name          string
		taskAtespace  string
		wsAtespace    string // where the "data" Workspace exists; empty for nowhere
		binding       string
		getErr        error
		wantCode      codes.Code
		wantMsg       string
		wantWorkspace bool
	}{
		{name: "missing", binding: "data", wantCode: codes.FailedPrecondition, wantMsg: `workspace "data" not found in atespace "default"`},
		{name: "other atespace", taskAtespace: "team-a", wsAtespace: "team-b", binding: "data", wantCode: codes.FailedPrecondition, wantMsg: `workspace "data" not found in atespace "team-a"`},
		{name: "wrapped not found", binding: "data", getErr: fmt.Errorf("lookup: %w", store.ErrNotFound), wantCode: codes.FailedPrecondition},
		{name: "store error", binding: "data", getErr: errors.New("store unavailable"), wantCode: codes.Internal, wantMsg: `getting workspace "data": store unavailable`},
		{name: "present", wsAtespace: "default", binding: "data", wantCode: codes.OK, wantWorkspace: true},
		{name: "present in task atespace", taskAtespace: "team-a", wsAtespace: "team-a", binding: "data", wantCode: codes.OK, wantWorkspace: true},
		{name: "no binding", wantCode: codes.OK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			mem := memory.NewStore()
			var st store.Store = mem
			if tc.getErr != nil {
				st = &failGetWorkspaceStore{Store: mem, err: tc.getErr}
			}
			if tc.wsAtespace != "" {
				if err := mem.SaveWorkspace(ctx, &v1alpha1.Workspace{
					Metadata: &v1alpha1.ObjectMeta{Atespace: tc.wsAtespace, Name: "data"},
					Spec:     &v1alpha1.WorkspaceSpec{},
				}); err != nil {
					t.Fatalf("SaveWorkspace: %v", err)
				}
			}
			sub := substratetest.NewServer(t)
			rec := newTestTaskManager(sub)
			srv := server.NewServer(st, rec)

			task := &v1alpha1.Task{
				Metadata: &v1alpha1.ObjectMeta{Atespace: tc.taskAtespace, Name: "task"},
				Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
			}
			if tc.binding != "" {
				task.Spec.Workspace = &v1alpha1.WorkspaceRef{Name: tc.binding}
			}
			_, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: task})
			if status.Code(err) != tc.wantCode {
				t.Fatalf("CreateTask: got %v, want %v", err, tc.wantCode)
			}
			if !strings.Contains(status.Convert(err).Message(), tc.wantMsg) {
				t.Errorf("CreateTask error = %q, want it to contain %q", status.Convert(err).Message(), tc.wantMsg)
			}

			atespace := cmp.Or(tc.taskAtespace, "default")
			_, getErr := mem.GetTask(ctx, atespace, "task")
			actor, hasActor := sub.Actor(atespace, "task")
			if tc.wantCode != codes.OK {
				if !errors.Is(getErr, store.ErrNotFound) {
					t.Errorf("stored task after rejection: got %v, want ErrNotFound", getErr)
				}
				if hasActor {
					t.Errorf("rejected task has actor %v, want none", actor)
				}
				return
			}
			if getErr != nil {
				t.Fatalf("stored task: %v", getErr)
			}
			if !hasActor {
				t.Fatal("accepted task has no actor")
			}
			ref := actor.GetActorTemplate()
			if _, ok := sub.Template(ref.GetAtespace(), ref.GetName()); !ok {
				t.Fatalf("actor template %v not found", ref)
			}
		})
	}
}

type failGetGatewayStore struct {
	store.Store
	err error
}

func (f *failGetGatewayStore) GetGateway(context.Context, string, string) (*v1alpha1.Gateway, error) {
	return nil, f.err
}

// A task gets its Gateway's rules when it is created, so CreateTask must not
// save or provision a task whose Gateway does not exist.
func TestCreateTask_RequiresBoundGateway(t *testing.T) {
	for _, tc := range []struct {
		name         string
		taskAtespace string
		gwAtespace   string // atespace that has the "gw" Gateway; empty if none does
		getErr       error
		wantCode     codes.Code
		wantMsg      string
	}{
		{name: "missing", wantCode: codes.FailedPrecondition, wantMsg: `gateway "gw" not found in atespace "default"`},
		{name: "store error", getErr: errors.New("store unavailable"), wantCode: codes.Internal, wantMsg: `getting gateway "gw": store unavailable`},
		{name: "present", taskAtespace: "team-a", gwAtespace: "team-a", wantCode: codes.OK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			mem := memory.NewStore()
			var st store.Store = mem
			if tc.getErr != nil {
				st = &failGetGatewayStore{Store: mem, err: tc.getErr}
			}
			const credentialURI = "ate-secret://k8s.io/default/default/token/value"
			if tc.gwAtespace != "" {
				if err := mem.SaveGateway(ctx, &v1alpha1.Gateway{
					Metadata: &v1alpha1.ObjectMeta{Atespace: tc.gwAtespace, Name: "gw"},
					Spec: &v1alpha1.GatewaySpec{Egress: []*ateapipb.EgressRule{{Https: &ateapipb.HttpsRule{
						Hostnames: []string{"api.example.com"},
						Effects: &ateapipb.HttpRuleEffects{ReplaceHeaders: []*ateapipb.CredentialHeader{{
							Header:        "Authorization",
							CredentialUri: credentialURI,
						}}},
					}}}},
				}); err != nil {
					t.Fatalf("SaveGateway: %v", err)
				}
			}
			sub := substratetest.NewServer(t)
			rec := newTestTaskManager(sub)
			srv := server.NewServer(st, rec)

			_, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
				Metadata: &v1alpha1.ObjectMeta{Atespace: tc.taskAtespace, Name: "task"},
				Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}, Gateway: &v1alpha1.GatewayRef{Name: "gw"}},
			}})
			if status.Code(err) != tc.wantCode {
				t.Fatalf("CreateTask: got %v, want %v", err, tc.wantCode)
			}
			if !strings.Contains(status.Convert(err).Message(), tc.wantMsg) {
				t.Errorf("CreateTask error = %q, want it to contain %q", status.Convert(err).Message(), tc.wantMsg)
			}

			atespace := cmp.Or(tc.taskAtespace, "default")
			_, getErr := mem.GetTask(ctx, atespace, "task")
			actor, hasActor := sub.Actor(atespace, "task")
			if tc.wantCode != codes.OK {
				if !errors.Is(getErr, store.ErrNotFound) {
					t.Errorf("stored task after rejection: got %v, want ErrNotFound", getErr)
				}
				if hasActor {
					t.Errorf("rejected task has actor %v, want none", actor)
				}
				return
			}
			if getErr != nil {
				t.Fatalf("stored task: %v", getErr)
			}
			policy, _ := sub.EgressPolicy(atespace, "task")
			if rules := policy.GetRules(); len(rules) != 1 || rules[0].GetHttps().GetEffects().GetReplaceHeaders()[0].GetCredentialUri() != credentialURI {
				t.Errorf("actor's rules = %v, want the gateway's", rules)
			}
			// The credential URI must not be passed into the sandbox.
			ref := actor.GetActorTemplate()
			tmpl, ok := sub.Template(ref.GetAtespace(), ref.GetName())
			if !ok {
				t.Fatalf("actor template %v not found", ref)
			}
			for _, env := range tmpl.GetContainers()[0].GetEnv() {
				if strings.Contains(env.GetValue(), credentialURI) {
					t.Errorf("%s holds the gateway's credential URI", env.GetName())
				}
			}
		})
	}
}

// A task gets its gateway's rules before it can run. If Substrate refuses
// them, the task fails and cannot be resumed. Once the task has them, later
// changes to the gateway do not affect it.
func TestServer_GatewayRulesAreCopiedOnce(t *testing.T) {
	ctx := context.Background()
	mem := memory.NewStore()
	sub := substratetest.NewServer(t)
	srv := server.NewServer(mem, newTestTaskManager(sub))
	saveGateway := func(hostname string) {
		t.Helper()
		if err := mem.SaveGateway(ctx, &v1alpha1.Gateway{
			Metadata: &v1alpha1.ObjectMeta{Name: "gw"},
			Spec: &v1alpha1.GatewaySpec{Egress: []*ateapipb.EgressRule{{
				Https: &ateapipb.HttpsRule{Hostnames: []string{hostname}},
			}}},
		}); err != nil {
			t.Fatalf("SaveGateway: %v", err)
		}
	}
	hostname := func() string {
		t.Helper()
		policy, ok := sub.EgressPolicy("default", "task")
		if !ok {
			return ""
		}
		return policy.GetRules()[0].GetHttps().GetHostnames()[0]
	}
	suspend := &v1alpha1.SuspendTaskRequest{Name: "task"}
	resume := &v1alpha1.ResumeTaskRequest{Name: "task"}

	const substrateMsg = `egress_policy.rules[0].https.hostnames[0]: Invalid value: "a.example.com": refused`
	sub.SetErrors(substratetest.Errors{CreateEgressPolicy: status.Error(codes.InvalidArgument, substrateMsg)})
	saveGateway("a.example.com")
	_, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "task"},
		Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}, Gateway: &v1alpha1.GatewayRef{Name: "gw"}},
	}})
	if msg := status.Convert(err).Message(); !strings.Contains(msg, `binding gateway "gw"`) || !strings.Contains(msg, substrateMsg) {
		t.Fatalf("CreateTask error = %v, want it to name the gateway and carry Substrate's message", err)
	}
	stored, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "task"})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	// The actor was provisioned before the refusal, so its state is reported.
	if got := stored.GetStatus().GetState(); got != v1alpha1.TaskState_TASK_STATE_SUSPENDED {
		t.Errorf("after refusal: state %v, want SUSPENDED", got)
	}

	// Resuming the task fails as well.
	if _, err := srv.ResumeTask(ctx, resume); err == nil {
		t.Fatal("ResumeTask succeeded although the gateway's rules were refused")
	}
	// Resuming the task also fails while the gateway is deleted, because
	// there are no rules to copy.
	sub.SetErrors(substratetest.Errors{})
	if err := mem.DeleteGateway(ctx, "default", "gw"); err != nil {
		t.Fatalf("DeleteGateway: %v", err)
	}
	_, err = srv.ResumeTask(ctx, resume)
	if msg := status.Convert(err).Message(); !strings.Contains(msg, `binding gateway "gw": not available in atespace "default"`) {
		t.Fatalf("ResumeTask error = %v, want it to say that the gateway is not available", err)
	}
	if actor, _ := sub.Actor("default", "task"); actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_RUNNING {
		t.Fatal("task is running without its gateway's rules")
	}

	// Once the gateway exists and Substrate accepts its rules, resuming copies
	// them and starts the task.
	saveGateway("a.example.com")
	if _, err := srv.ResumeTask(ctx, resume); err != nil {
		t.Fatalf("ResumeTask: %v", err)
	}
	if got := hostname(); got != "a.example.com" {
		t.Fatalf("actor's rule after resume = %q, want a.example.com", got)
	}

	// From then on the task keeps these rules. Editing or deleting the gateway
	// does not change them or stop a suspend or resume, even when Substrate
	// would refuse the new rules.
	sub.SetErrors(substratetest.Errors{CreateEgressPolicy: status.Error(codes.InvalidArgument, substrateMsg)})
	saveGateway("b.example.com")
	if _, err := srv.SuspendTask(ctx, suspend); err != nil {
		t.Fatalf("SuspendTask after the gateway changed: %v", err)
	}
	if _, err := srv.ResumeTask(ctx, resume); err != nil {
		t.Fatalf("ResumeTask after the gateway changed: %v", err)
	}
	if err := mem.DeleteGateway(ctx, "default", "gw"); err != nil {
		t.Fatalf("DeleteGateway: %v", err)
	}
	if _, err := srv.SuspendTask(ctx, suspend); err != nil {
		t.Fatalf("SuspendTask after the gateway was deleted: %v", err)
	}
	if _, err := srv.ResumeTask(ctx, resume); err != nil {
		t.Fatalf("ResumeTask after the gateway was deleted: %v", err)
	}
	if got := hostname(); got != "a.example.com" {
		t.Errorf("actor's rule after the gateway changed = %q, want a.example.com", got)
	}
}

// Suspending a running task that lacks its gateway's rules stops its actor.
// The suspend still fails when the rules cannot be copied.
func TestServer_SuspendTask_RunningTaskWithoutGatewayRules(t *testing.T) {
	ctx := context.Background()
	mem := memory.NewStore()
	sub := substratetest.NewServer(t)
	srv := server.NewServer(mem, newTestTaskManager(sub))
	if err := mem.SaveGateway(ctx, &v1alpha1.Gateway{
		Metadata: &v1alpha1.ObjectMeta{Name: "gw"},
		Spec: &v1alpha1.GatewaySpec{Egress: []*ateapipb.EgressRule{{
			Https: &ateapipb.HttpsRule{Hostnames: []string{"a.example.com"}},
		}}},
	}); err != nil {
		t.Fatalf("SaveGateway: %v", err)
	}

	sub.SetErrors(substratetest.Errors{CreateEgressPolicy: status.Error(codes.InvalidArgument, "refused")})
	if _, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "task"},
		Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}, Gateway: &v1alpha1.GatewayRef{Name: "gw"}},
	}}); err == nil {
		t.Fatal("CreateTask succeeded although the gateway's rules were refused")
	}

	// Substrate may start an actor on its own, for example for a request.
	actor, _ := sub.Actor("default", "task")
	actor.Status.State = ateapipb.ActorState_ACTOR_STATE_RUNNING
	sub.SetActor(actor)
	_, err := srv.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{Name: "task"})
	if msg := status.Convert(err).Message(); !strings.Contains(msg, `binding gateway "gw"`) {
		t.Errorf("SuspendTask error = %v, want it to report that the gateway was not bound", err)
	}
	if actor, _ := sub.Actor("default", "task"); actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Errorf("actor state after SuspendTask = %v, want suspended", actor.GetStatus().GetState())
	}
}

// A task whose creation failed before its gateway's rules were copied gets
// them when it is next suspended or resumed.
func TestServer_PendingTaskGetsGatewayRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		next func(context.Context, *server.Server) error
	}{
		{name: "suspend", next: func(ctx context.Context, srv *server.Server) error {
			_, err := srv.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{Name: "task"})
			return err
		}},
		{name: "resume", next: func(ctx context.Context, srv *server.Server) error {
			_, err := srv.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Name: "task"})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			mem := memory.NewStore()
			sub := substratetest.NewServer(t)
			srv := server.NewServer(mem, newTestTaskManager(sub))
			if err := mem.SaveGateway(ctx, &v1alpha1.Gateway{
				Metadata: &v1alpha1.ObjectMeta{Name: "gw"},
				Spec: &v1alpha1.GatewaySpec{Egress: []*ateapipb.EgressRule{{
					Https: &ateapipb.HttpsRule{Hostnames: []string{"a.example.com"}},
				}}},
			}); err != nil {
				t.Fatalf("SaveGateway: %v", err)
			}

			sub.SetErrors(substratetest.Errors{CreateEgressPolicy: status.Error(codes.DeadlineExceeded, "substrate control plane timed out")})
			_, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
				Metadata: &v1alpha1.ObjectMeta{Name: "task"},
				Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}, Gateway: &v1alpha1.GatewayRef{Name: "gw"}},
			}})
			if status.Code(err) != codes.Unavailable {
				t.Fatalf("CreateTask: got %v, want Unavailable", err)
			}
			stored, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "task"})
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if reason := readyCondition(stored).GetReason(); reason != v1alpha1.ReasonSubstrateUnavailable {
				t.Errorf("Ready reason after the failed creation = %q, want %q", reason, v1alpha1.ReasonSubstrateUnavailable)
			}

			sub.SetErrors(substratetest.Errors{})
			if err := tc.next(ctx, srv); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if policy, _ := sub.EgressPolicy("default", "task"); len(policy.GetRules()) != 1 {
				t.Errorf("actor's rules after %s = %v, want the gateway's", tc.name, policy.GetRules())
			}
		})
	}
}

// A resume or suspend fails while the store cannot read the task's Workspace
// or Gateway, and does not start the task's actor. A Workspace that no longer
// exists is not an error.
func TestServer_ResumeAndSuspendWithStoreError(t *testing.T) {
	ctx := context.Background()
	mem := memory.NewStore()
	sub := substratetest.NewServer(t)
	if err := mem.SaveWorkspace(ctx, &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "ws"},
		Spec:     &v1alpha1.WorkspaceSpec{},
	}); err != nil {
		t.Fatalf("SaveWorkspace: %v", err)
	}
	if err := mem.SaveGateway(ctx, &v1alpha1.Gateway{
		Metadata: &v1alpha1.ObjectMeta{Name: "gw"},
		Spec: &v1alpha1.GatewaySpec{Egress: []*ateapipb.EgressRule{{
			Https: &ateapipb.HttpsRule{Hostnames: []string{"a.example.com"}},
		}}},
	}); err != nil {
		t.Fatalf("SaveGateway: %v", err)
	}
	if _, err := server.NewServer(mem, newTestTaskManager(sub)).CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "task"},
		Spec: &v1alpha1.TaskSpec{
			Profile:   &v1alpha1.Profile{Image: "alpine"},
			Workspace: &v1alpha1.WorkspaceRef{Name: "ws"},
			Gateway:   &v1alpha1.GatewayRef{Name: "gw"},
		},
	}}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	storeErr := errors.New("store unavailable")
	for _, tc := range []struct {
		st      store.Store
		wantMsg string
	}{
		{&failGetWorkspaceStore{Store: mem, err: storeErr}, `getting workspace "ws": store unavailable`},
		{&failGetGatewayStore{Store: mem, err: storeErr}, `getting gateway "gw": store unavailable`},
	} {
		srv := server.NewServer(tc.st, newTestTaskManager(sub))
		if _, err := srv.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Name: "task"}); status.Code(err) != codes.Internal || status.Convert(err).Message() != tc.wantMsg {
			t.Errorf("ResumeTask: got %v, want Internal %q", err, tc.wantMsg)
		}
		if _, err := srv.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{Name: "task"}); status.Code(err) != codes.Internal || status.Convert(err).Message() != tc.wantMsg {
			t.Errorf("SuspendTask: got %v, want Internal %q", err, tc.wantMsg)
		}
	}
	if actor, _ := sub.Actor("default", "task"); actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Errorf("actor state after the failed requests = %v, want suspended", actor.GetStatus().GetState())
	}

	if err := mem.DeleteWorkspace(ctx, "default", "ws"); err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}
	srv := server.NewServer(mem, newTestTaskManager(sub))
	if _, err := srv.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Name: "task"}); err != nil {
		t.Errorf("ResumeTask: %v", err)
	}
	if _, err := srv.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{Name: "task"}); err != nil {
		t.Errorf("SuspendTask: %v", err)
	}
}

// CreateGateway makes a new gateway and UpdateGateway replaces an existing
// one; neither does the other's work.
func TestServer_CreateAndUpdateGateway(t *testing.T) {
	srv := server.NewServer(memory.NewStore(), newTestTaskManager(substratetest.NewServer(t)))
	ctx := context.Background()
	// Every request carries timestamps, as a manifest copied from
	// `ax get gateway` does. The server keeps its own.
	copied := timestamppb.New(time.Unix(1, 0))
	gateway := func(hostname string) *v1alpha1.Gateway {
		return &v1alpha1.Gateway{
			Metadata: &v1alpha1.ObjectMeta{Name: "gw", CreateTime: copied, UpdateTime: copied, DeleteTime: copied},
			Spec: &v1alpha1.GatewaySpec{Egress: []*ateapipb.EgressRule{{
				Https: &ateapipb.HttpsRule{Hostnames: []string{hostname}},
			}}},
		}
	}

	if _, err := srv.UpdateGateway(ctx, &v1alpha1.UpdateGatewayRequest{Gateway: gateway("a.example.com")}); status.Code(err) != codes.NotFound {
		t.Fatalf("UpdateGateway before create: got %v, want NotFound", err)
	}
	created, err := srv.CreateGateway(ctx, &v1alpha1.CreateGatewayRequest{Gateway: gateway("a.example.com")})
	if err != nil {
		t.Fatalf("CreateGateway: %v", err)
	}
	if _, err := srv.CreateGateway(ctx, &v1alpha1.CreateGatewayRequest{Gateway: gateway("b.example.com")}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("CreateGateway again: got %v, want AlreadyExists", err)
	}
	if _, err := srv.UpdateGateway(ctx, &v1alpha1.UpdateGatewayRequest{Gateway: gateway("b.example.com")}); err != nil {
		t.Fatalf("UpdateGateway: %v", err)
	}

	got, err := srv.GetGateway(ctx, &v1alpha1.GetGatewayRequest{Name: "gw"})
	if err != nil {
		t.Fatalf("GetGateway: %v", err)
	}
	if hostname := got.GetSpec().GetEgress()[0].GetHttps().GetHostnames()[0]; hostname != "b.example.com" {
		t.Errorf("hostname after update = %q, want b.example.com", hostname)
	}
	if meta := created.GetMetadata(); meta.GetCreateTime().AsTime().Equal(copied.AsTime()) || !meta.GetUpdateTime().AsTime().Equal(meta.GetCreateTime().AsTime()) {
		t.Errorf("expected CreateGateway to assign CreateTime and UpdateTime, got %v and %v", meta.GetCreateTime(), meta.GetUpdateTime())
	}
	if !got.GetMetadata().GetCreateTime().AsTime().Equal(created.GetMetadata().GetCreateTime().AsTime()) {
		t.Errorf("expected CreateTime to be preserved across update")
	}
	if meta := got.GetMetadata(); meta.GetUpdateTime().AsTime().Before(meta.GetCreateTime().AsTime()) || meta.GetDeleteTime() != nil {
		t.Errorf("expected UpdateGateway to assign UpdateTime and ignore DeleteTime, got %v and %v", meta.GetUpdateTime(), meta.GetDeleteTime())
	}
}

func TestServer_DirectTaskManagerLifecycle(t *testing.T) {
	sub := substratetest.NewServer(t)
	srv := server.NewServer(memory.NewStore(), newTestTaskManager(sub))
	ctx := context.Background()

	// 1. CreateTask creates the task's actor, suspended
	task, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "task-rec"},
			Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
		},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	actor, _ := sub.Actor("default", "task-rec")
	if state := actor.GetStatus().GetState(); state != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Errorf("actor state after CreateTask = %v, want SUSPENDED", state)
	}
	tmpl := actor.GetActorTemplate()
	if _, ok := sub.Template(tmpl.GetAtespace(), tmpl.GetName()); !ok {
		t.Errorf("expected ActorTemplate %s/%s to be created", tmpl.GetAtespace(), tmpl.GetName())
	}
	if task.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_SUSPENDED {
		t.Errorf("expected state SUSPENDED, got %v", task.GetStatus().GetState())
	}

	// 2. ResumeTask resumes the actor
	task, err = srv.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Name: "task-rec"})
	if err != nil {
		t.Fatalf("ResumeTask: %v", err)
	}
	actor, _ = sub.Actor("default", "task-rec")
	if state := actor.GetStatus().GetState(); state != ateapipb.ActorState_ACTOR_STATE_RUNNING {
		t.Errorf("actor state after ResumeTask = %v, want RUNNING", state)
	}
	if task.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_RUNNING {
		t.Errorf("expected state RUNNING, got %v", task.GetStatus().GetState())
	}

	// 3. SuspendTask suspends the actor
	task, err = srv.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{Name: "task-rec"})
	if err != nil {
		t.Fatalf("SuspendTask: %v", err)
	}
	actor, _ = sub.Actor("default", "task-rec")
	if state := actor.GetStatus().GetState(); state != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Errorf("actor state after SuspendTask = %v, want SUSPENDED", state)
	}
	if task.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_SUSPENDED {
		t.Errorf("expected state SUSPENDED, got %v", task.GetStatus().GetState())
	}

	// 4. DeleteTask deletes the actor and preserves shared ax-* profile templates.
	_, err = srv.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Name: "task-rec"})
	if err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	if _, ok := sub.Actor("default", "task-rec"); ok {
		t.Error("expected the actor to be deleted")
	}
	if _, ok := sub.Template(tmpl.GetAtespace(), tmpl.GetName()); !ok {
		t.Errorf("expected shared profile ActorTemplate %s/%s to be preserved", tmpl.GetAtespace(), tmpl.GetName())
	}

	// 5. Verify task is gone
	_, err = srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "task-rec"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound after DeleteTask, got %v", err)
	}
}

func TestServer_DeleteTask_BlocksAndSetsDeleteTime(t *testing.T) {
	st := memory.NewStore()
	var observedDeleteTimeDuringDelete bool
	sub := substratetest.NewServer(t)
	sub.SetOnDeleteActor(func() {
		task, err := st.GetTask(context.Background(), "default", "task-term")
		if err == nil && task.GetMetadata().GetDeleteTime() != nil {
			observedDeleteTimeDuringDelete = true
		}
	})
	rec := newTestTaskManager(sub)
	srv := server.NewServer(st, rec)
	ctx := context.Background()

	_, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "task-term"},
			Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
		},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	_, err = srv.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Name: "task-term"})
	if err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}

	if !observedDeleteTimeDuringDelete {
		t.Error("expected metadata.delete_time to be set during delete")
	}

	_, err = srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "task-term"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound after successful DeleteTask, got %v", err)
	}
}

func TestServer_DeleteTask_ReconcileErrorRetainsTask(t *testing.T) {
	st := memory.NewStore()
	sub := substratetest.NewServer(t)
	rec := newTestTaskManager(sub)
	srv := server.NewServer(st, rec)
	ctx := context.Background()

	_, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "task-err"},
			Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
		},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	sub.SetErrors(substratetest.Errors{DeleteActor: status.Error(codes.Internal, "substrate failure deleting actor")})
	_, err = srv.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Name: "task-err"})
	if err == nil {
		t.Fatal("expected DeleteTask to fail when Substrate fails to delete the actor")
	}

	task, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "task-err"})
	if err != nil {
		t.Fatalf("expected task to remain in store, got %v", err)
	}
	if task.GetMetadata().GetDeleteTime() == nil {
		t.Error("expected metadata.delete_time to be set after failed delete")
	}
	if got := v1alpha1.FormatTaskState(task); got != v1alpha1.ReasonTerminating {
		t.Errorf("FormatTaskState = %q, want %q", got, v1alpha1.ReasonTerminating)
	}
}

type failSaveTaskStore struct {
	store.Store
	failSave bool
}

func (f *failSaveTaskStore) SaveTask(ctx context.Context, task *v1alpha1.Task) error {
	if f.failSave {
		return errors.New("simulated store failure")
	}
	return f.Store.SaveTask(ctx, task)
}

func TestServer_DeleteTask_SetDeleteTimeError(t *testing.T) {
	st := &failSaveTaskStore{Store: memory.NewStore()}
	sub := substratetest.NewServer(t)
	rec := newTestTaskManager(sub)
	srv := server.NewServer(st, rec)
	ctx := context.Background()

	_, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "task-status-fail"},
			Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
		},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	st.failSave = true
	_, err = srv.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Name: "task-status-fail"})
	if err == nil {
		t.Fatal("expected DeleteTask to fail when persisting delete_time fails")
	}
	if sub.DeleteActorCalls() != 0 {
		t.Errorf("expected no DeleteActor call if persisting delete_time fails, got %d", sub.DeleteActorCalls())
	}
}

func TestServer_ResumeAfterFailedDeleteClearsDeleteTime(t *testing.T) {
	st := memory.NewStore()
	sub := substratetest.NewServer(t)
	srv := server.NewServer(st, newTestTaskManager(sub))
	ctx := context.Background()

	if _, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "task-cancel-del"},
			Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
		},
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	sub.SetErrors(substratetest.Errors{DeleteActor: status.Error(codes.Internal, "substrate failure")})
	if _, err := srv.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Name: "task-cancel-del"}); err == nil {
		t.Fatal("expected DeleteTask to fail")
	}
	sub.SetErrors(substratetest.Errors{})

	resumed, err := srv.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Name: "task-cancel-del"})
	if err != nil {
		t.Fatalf("ResumeTask: %v", err)
	}
	if resumed.GetMetadata().GetDeleteTime() != nil {
		t.Errorf("expected metadata.delete_time cleared, got %v", resumed.GetMetadata().GetDeleteTime())
	}
	if resumed.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_RUNNING {
		t.Errorf("resumed state = %v, want RUNNING", resumed.GetStatus().GetState())
	}

	stored, err := st.GetTask(ctx, "default", "task-cancel-del")
	if err != nil {
		t.Fatalf("store.GetTask: %v", err)
	}
	if stored.GetMetadata().GetDeleteTime() != nil {
		t.Errorf("stored delete_time = %v, want nil", stored.GetMetadata().GetDeleteTime())
	}
	assertNoActorState(t, stored)

	got, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "task-cancel-del"})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_RUNNING {
		t.Errorf("GetTask state = %v, want RUNNING", got.GetStatus().GetState())
	}
}

func TestServer_SuspendTask_ReconcileErrorRetainsRunningState(t *testing.T) {
	// The task's runner reports its workspace as ready, so the running task is Ready.
	readySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer readySrv.Close()
	t.Setenv("ATENET_ROUTER_ADDR", readySrv.URL)

	st := memory.NewStore()
	sub := substratetest.NewServer(t)
	rec := newTestTaskManager(sub)
	srv := server.NewServer(st, rec)
	ctx := context.Background()

	if _, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "task-suspend-err"},
		Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
	}}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	running, err := srv.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Name: "task-suspend-err"})
	if err != nil {
		t.Fatalf("ResumeTask: %v", err)
	}
	if ready := readyCondition(running); running.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_RUNNING || ready.GetStatus() != "True" {
		t.Fatalf("ResumeTask status = %v, want RUNNING with Ready=True", running.GetStatus())
	}

	sub.SetErrors(substratetest.Errors{SuspendActor: status.Error(codes.Internal, "substrate suspend error")})
	_, err = srv.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{Name: "task-suspend-err"})
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal error when Substrate fails to suspend the actor, got %v", err)
	}

	stored, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "task-suspend-err"})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_RUNNING {
		t.Errorf("stored state = %v, want RUNNING after failed suspension", stored.GetStatus().GetState())
	}
	if ready := readyCondition(stored); ready.GetStatus() != "True" || ready.GetReason() != v1alpha1.ReasonTaskRunning {
		t.Errorf("stored conditions = %v, want Ready=True (TaskRunning)", stored.GetStatus().GetConditions())
	}
}

func TestServer_SuspendTask_SuccessUpdatesStoredState(t *testing.T) {
	st := memory.NewStore()
	srv := server.NewServer(st, newTestTaskManager(substratetest.NewServer(t)))
	ctx := context.Background()

	if _, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "task-suspend-ok"},
		Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
	}}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if _, err := srv.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Name: "task-suspend-ok"}); err != nil {
		t.Fatalf("ResumeTask: %v", err)
	}

	resp, err := srv.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{Name: "task-suspend-ok"})
	if err != nil {
		t.Fatalf("SuspendTask: %v", err)
	}
	if resp.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_SUSPENDED {
		t.Errorf("response state = %v, want SUSPENDED", resp.GetStatus().GetState())
	}

	stored, err := st.GetTask(ctx, "default", "task-suspend-ok")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	assertNoActorState(t, stored)
}

// assertNoActorState checks that a stored task holds none of the status that
// belongs to its actor on Substrate.
func assertNoActorState(t *testing.T, stored *v1alpha1.Task) {
	t.Helper()
	st := stored.GetStatus()
	if st.GetState() != v1alpha1.TaskState_TASK_STATE_UNSPECIFIED || st.GetActor() != "" {
		t.Errorf("stored status has state=%v actor=%q, want UNSPECIFIED and empty", st.GetState(), st.GetActor())
	}
	for _, c := range st.GetConditions() {
		if c.GetType() == "Ready" {
			t.Errorf("stored status has a Ready condition %v, want none", c)
		}
	}
}

// newTestTaskManager returns a TaskManager that runs tasks on sub.
func newTestTaskManager(sub *substratetest.Server) *controller.TaskManager {
	return controller.NewTaskManager(sub.Client(), "test-template", v1alpha1.DefaultSystemNamespace)
}

// readyCondition returns the task's Ready condition, or nil.
func readyCondition(task *v1alpha1.Task) *v1alpha1.Condition {
	for _, c := range task.GetStatus().GetConditions() {
		if c.GetType() == v1alpha1.ConditionReady {
			return c
		}
	}
	return nil
}

func TestServer_ReadsActorStateFromSubstrate(t *testing.T) {
	st := memory.NewStore()
	sub := substratetest.NewServer(t)
	rec := newTestTaskManager(sub)
	srv := server.NewServer(st, rec)
	ctx := context.Background()

	for _, name := range []string{"observed-a", "observed-b"} {
		if _, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: name},
			Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
		}}); err != nil {
			t.Fatalf("CreateTask(%s): %v", name, err)
		}
		// Substrate runs the actor without AX asking it to.
		sub.SetActor(&ateapipb.Actor{
			Metadata: &ateapipb.ResourceMetadata{Atespace: "default", Name: name},
			Status: &ateapipb.ActorStatus{
				State:            ateapipb.ActorState_ACTOR_STATE_RUNNING,
				WorkerAssignment: &ateapipb.WorkerAssignment{WorkerPodIps: []string{"10.244.1.7"}},
			},
		})
	}

	got, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "observed-a"})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_RUNNING || got.GetStatus().GetActor() != "observed-a" {
		t.Errorf("GetTask status = %v, want the actor's state", got.GetStatus())
	}

	list, err := srv.ListTasks(ctx, &v1alpha1.ListTasksRequest{Atespace: "default"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	for _, task := range list.GetTasks() {
		if task.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_RUNNING || task.GetStatus().GetActor() != task.GetMetadata().GetName() {
			t.Errorf("ListTasks status for %s = %v, want the actor's state", task.GetMetadata().GetName(), task.GetStatus())
		}
	}

	// A change on Substrate alone is visible on the next read.
	sub.SetActor(&ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "default", Name: "observed-a"},
		Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED},
	})
	got, err = srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "observed-a"})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_SUSPENDED {
		t.Errorf("state after the actor was suspended on Substrate = %v, want SUSPENDED", got.GetStatus().GetState())
	}

	stored, err := st.GetTask(ctx, "default", "observed-a")
	if err != nil {
		t.Fatalf("store GetTask: %v", err)
	}
	assertNoActorState(t, stored)
}

func TestServer_CreateTask_InitialStateSuspended(t *testing.T) {
	st := memory.NewStore()
	srv := server.NewServer(st, newTestTaskManager(substratetest.NewServer(t)))
	ctx := context.Background()

	task, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "task-create-suspended"},
			Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
		},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_SUSPENDED {
		t.Errorf("returned task state = %v, want SUSPENDED", task.GetStatus().GetState())
	}

	stored, err := st.GetTask(ctx, "default", "task-create-suspended")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	assertNoActorState(t, stored)
}

func TestServer_CreateTask_ReconcileErrorMarksFailed(t *testing.T) {
	st := memory.NewStore()
	sub := substratetest.NewServer(t)
	sub.SetErrors(substratetest.Errors{SuspendActor: status.Error(codes.Internal, "substrate suspend actor failure")})
	rec := newTestTaskManager(sub)
	srv := server.NewServer(st, rec)
	ctx := context.Background()

	_, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "task-create-fail"},
			Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
		},
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal error when Substrate fails to suspend the new actor, got %v", err)
	}

	// Task was saved, and its state is read from the actor that was provisioned
	stored, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "task-create-fail"})
	if err != nil {
		t.Fatalf("expected task to exist in store after a failed create, got %v", err)
	}
	if stored.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_SUSPENDED {
		t.Errorf("state = %v, want SUSPENDED", stored.GetStatus().GetState())
	}

	// Because task already exists in store, attempting to create it again must fail with FailedPrecondition
	_, err = srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "task-create-fail"},
			Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
		},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition on duplicate create, got %v", err)
	}
}

// TestCreateTask_IgnoresServerAssignedFields creates a task from a manifest
// copied from `ax get task`, which carries metadata.uid, the timestamps, and
// status. The server must assign its own.
func TestCreateTask_IgnoresServerAssignedFields(t *testing.T) {
	sub := substratetest.NewServer(t)
	// Provisioning fails so that the task reads back as CreateTask stored it; a
	// successful CreateTask clears deleteTime on its own.
	sub.SetErrors(substratetest.Errors{SuspendActor: status.Error(codes.Internal, "substrate suspend actor failure")})
	srv := server.NewServer(memory.NewStore(), newTestTaskManager(sub))
	ctx := context.Background()

	copied := timestamppb.New(time.Unix(1, 0))
	_, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "task-copy", Uid: "copied-uid", CreateTime: copied, DeleteTime: copied},
			Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
			Status:   &v1alpha1.TaskStatus{Attempt: 7},
		},
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal error when Substrate fails to suspend the new actor, got %v", err)
	}

	task, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "task-copy"})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	meta := task.GetMetadata()
	if meta.GetUid() == "" || meta.GetUid() == "copied-uid" {
		t.Errorf("uid = %q, want a server-assigned uid", meta.GetUid())
	}
	if meta.GetCreateTime().AsTime().Equal(copied.AsTime()) || meta.GetDeleteTime() != nil {
		t.Errorf("createTime = %v, deleteTime = %v, want the client-supplied timestamps ignored", meta.GetCreateTime(), meta.GetDeleteTime())
	}
	if attempt := task.GetStatus().GetAttempt(); attempt != 1 {
		t.Errorf("status.attempt = %d, want 1", attempt)
	}
}

func TestServer_TransientSubstrateUnavailableRecovery(t *testing.T) {
	st := memory.NewStore()
	sub := substratetest.NewServer(t)
	sub.SetErrors(substratetest.Errors{CreateAtespace: status.Error(codes.DeadlineExceeded, "substrate control plane timed out")})
	rec := newTestTaskManager(sub)
	srv := server.NewServer(st, rec)
	ctx := context.Background()

	// 1. CreateTask while Substrate is unavailable records State=QUEUED (not FAILED)
	// and returns codes.Unavailable.
	_, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "simple-task"},
			Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
		},
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable from CreateTask when Substrate is unreachable, got %v", err)
	}

	stored, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "simple-task"})
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if stored.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_QUEUED {
		t.Errorf("expected stored state QUEUED after transient Substrate error, got %v", stored.GetStatus().GetState())
	}

	// 2. Re-applying a Queued task with a different spec is rejected to preserve immutability.
	_, err = srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "simple-task"},
			Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "debian"}},
		},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition when re-applying Queued task with modified spec, got %v", err)
	}

	// 3. Re-applying a Queued task with omitted status preserves stored status conditions
	// rather than dropping them before reconciliation. Steps 3 and 4 fail at SuspendActor,
	// which leaves the conditions it is given alone; a failing CreateAtespace would
	// overwrite Ready with SubstrateUnavailable whatever the server passed in.
	sub.SetErrors(substratetest.Errors{SuspendActor: status.Error(codes.DeadlineExceeded, "substrate timed out suspending actor")})
	_, err = srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "simple-task"},
			Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
		},
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable on retry while Substrate is still down, got %v", err)
	}
	retried, err := st.GetTask(ctx, "default", "simple-task")
	if err != nil {
		t.Fatalf("store GetTask: %v", err)
	}
	if conds := retried.GetStatus().GetConditions(); len(conds) != 1 || conds[0].GetReason() != v1alpha1.ReasonSubstrateUnavailable {
		t.Errorf("expected stored condition SubstrateUnavailable to be preserved on omitted-status retry, got %v", conds)
	}

	// 4. Re-applying a Queued task with client-supplied conditions reuses the stored task
	// rather than overwriting status with client-supplied values.
	_, err = srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "simple-task"},
			Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
			Status: &v1alpha1.TaskStatus{
				State: v1alpha1.TaskState_TASK_STATE_RUNNING,
				Conditions: []*v1alpha1.Condition{
					{Type: "Ready", Status: "True", Reason: "ClientForged"},
				},
			},
		},
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable on retry while Substrate is still down, got %v", err)
	}
	retried, err = st.GetTask(ctx, "default", "simple-task")
	if err != nil {
		t.Fatalf("store GetTask: %v", err)
	}
	if conds := retried.GetStatus().GetConditions(); len(conds) != 1 || conds[0].GetReason() != v1alpha1.ReasonSubstrateUnavailable {
		t.Errorf("expected client-supplied conditions to be ignored on Queued retry, got %v", conds)
	}

	// 5. Once Substrate recovers, re-applying CreateTask with the unchanged spec succeeds
	// for a Queued task and transitions it to Suspended.
	sub.SetErrors(substratetest.Errors{})
	created, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "simple-task"},
			Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
		},
	})
	if err != nil {
		t.Fatalf("CreateTask retry after Substrate recovery failed: %v", err)
	}
	if created.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_SUSPENDED {
		t.Errorf("expected state SUSPENDED after recovery, got %v", created.GetStatus().GetState())
	}
	if !created.GetMetadata().GetCreateTime().AsTime().Equal(stored.GetMetadata().GetCreateTime().AsTime()) {
		t.Errorf("expected CreateTime to be preserved across Queued re-apply")
	}
	if created.GetMetadata().GetUid() != stored.GetMetadata().GetUid() {
		t.Errorf("expected Uid to be preserved across Queued re-apply")
	}

	// 6. ResumeTask during a brief Substrate outage keeps the task Queued instead of Failed.
	sub.SetErrors(substratetest.Errors{ResumeActor: status.Error(codes.DeadlineExceeded, "substrate worker pool temporarily unreachable")})
	_, err = srv.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Name: "simple-task"})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable from ResumeTask when Substrate is unreachable, got %v", err)
	}
	storedAfterResumeErr, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "simple-task"})
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if storedAfterResumeErr.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_QUEUED {
		t.Errorf("expected stored state QUEUED after transient ResumeTask error, got %v", storedAfterResumeErr.GetStatus().GetState())
	}

	// 7. Because initial CreateTask already completed, re-applying CreateTask on a task
	// that entered QUEUED from a later ResumeTask failure is still rejected as immutable.
	sub.SetErrors(substratetest.Errors{})
	_, err = srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "simple-task"},
			Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
		},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition re-applying already-provisioned task after ResumeTask error, got %v", err)
	}
}

// Resuming and reverting actors map to TASK_STATE_RESUMING and
// TASK_STATE_REVERTING on read, and CreateTask must reject re-applying an
// already-created task rather than running the create path and suspending the
// resuming actor.
func TestCreateTask_RejectsResumingTask(t *testing.T) {
	for actorState, wantState := range map[ateapipb.ActorState]v1alpha1.TaskState{
		ateapipb.ActorState_ACTOR_STATE_RESUMING:  v1alpha1.TaskState_TASK_STATE_RESUMING,
		ateapipb.ActorState_ACTOR_STATE_REVERTING: v1alpha1.TaskState_TASK_STATE_REVERTING,
	} {
		t.Run(actorState.String(), func(t *testing.T) {
			st := memory.NewStore()
			sub := substratetest.NewServer(t)
			rec := newTestTaskManager(sub)
			srv := server.NewServer(st, rec)
			ctx := context.Background()

			spec := &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}}
			if _, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
				Metadata: &v1alpha1.ObjectMeta{Name: "resuming-task"},
				Spec:     spec,
			}}); err != nil {
				t.Fatalf("CreateTask: %v", err)
			}

			actor, ok := sub.Actor("default", "resuming-task")
			if !ok {
				t.Fatal("expected actor to exist")
			}
			actor.Status = &ateapipb.ActorStatus{State: actorState}
			sub.SetActor(actor)

			got, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "resuming-task"})
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if got.GetStatus().GetState() != wantState {
				t.Fatalf("GetTask state = %v, want %v", got.GetStatus().GetState(), wantState)
			}
			if ready := readyCondition(got); ready.GetReason() != v1alpha1.ReasonResuming {
				t.Fatalf("Ready = %v, want reason %q", ready, v1alpha1.ReasonResuming)
			}

			_, err = srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
				Metadata: &v1alpha1.ObjectMeta{Name: "resuming-task"},
				Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
			}})
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("CreateTask on %v actor: got %v, want FailedPrecondition", actorState, err)
			}

			after, _ := sub.Actor("default", "resuming-task")
			if after.GetStatus().GetState() != actorState {
				t.Errorf("actor state changed to %v after rejected CreateTask; want %v preserved", after.GetStatus().GetState(), actorState)
			}
		})
	}
}

// A task whose actor could not be created reports why in its Ready condition,
// not as a missing actor.
func TestServer_CreateTask_ActorCreationFailureKeepsReady(t *testing.T) {
	sub := substratetest.NewServer(t)
	srv := server.NewServer(memory.NewStore(), newTestTaskManager(sub))
	ctx := context.Background()

	sub.SetErrors(substratetest.Errors{CreateActor: status.Error(codes.InvalidArgument, "refused")})
	if _, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "task"},
		Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
	}}); status.Code(err) != codes.Internal {
		t.Fatalf("CreateTask: got %v, want Internal", err)
	}
	task, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "task"})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	ready := readyCondition(task)
	if task.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_UNSPECIFIED || ready.GetStatus() != "False" || ready.GetReason() != v1alpha1.ReasonActorCreationFailed {
		t.Errorf("state %v, Ready %v; want UNSPECIFIED with Ready False/%s", task.GetStatus().GetState(), ready, v1alpha1.ReasonActorCreationFailed)
	}
}

// A failed create stores the task's Ready condition. A later successful
// suspend or resume clears it, and the state is read from the actor.
func TestServer_SuccessClearsStoredFailure(t *testing.T) {
	sub := substratetest.NewServer(t)
	srv := server.NewServer(memory.NewStore(), newTestTaskManager(sub))
	ctx := context.Background()

	// createFailed creates a task whose actor exists but could not be
	// suspended.
	createFailed := func(name string) {
		t.Helper()
		sub.SetErrors(substratetest.Errors{SuspendActor: status.Error(codes.Internal, "substrate suspend failure")})
		defer sub.SetErrors(substratetest.Errors{})
		if _, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: name},
			Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
		}}); status.Code(err) != codes.Internal {
			t.Fatalf("CreateTask(%s): got %v, want Internal", name, err)
		}
	}

	createFailed("task-suspend")
	task, err := srv.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{Name: "task-suspend"})
	if err != nil {
		t.Fatalf("SuspendTask: %v", err)
	}
	if task.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_SUSPENDED {
		t.Errorf("state after SuspendTask = %v, want SUSPENDED", task.GetStatus().GetState())
	}

	createFailed("task-resume")
	task, err = srv.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Name: "task-resume"})
	if err != nil {
		t.Fatalf("ResumeTask: %v", err)
	}
	if task.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_RUNNING {
		t.Errorf("state after ResumeTask = %v, want RUNNING", task.GetStatus().GetState())
	}
}

func TestCreateTask_RejectsUnimplementedFields(t *testing.T) {
	st := memory.NewStore()
	sub := substratetest.NewServer(t)
	rec := newTestTaskManager(sub)
	srv := server.NewServer(st, rec)
	ctx := context.Background()

	tests := []struct {
		name      string
		spec      *v1alpha1.TaskSpec
		wantField string
	}{
		{
			name:      "retry",
			spec:      &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}, Retry: &v1alpha1.RetryConfig{MaxRetries: 2}},
			wantField: "spec.retry is not supported yet",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
				Metadata: &v1alpha1.ObjectMeta{Name: "unimpl-task"},
				Spec:     tc.spec,
			}})
			if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), tc.wantField) {
				t.Fatalf("CreateTask() = %v, want Unimplemented containing %q", err, tc.wantField)
			}
			if _, err := st.GetTask(ctx, "default", "unimpl-task"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("expected rejected task not to be saved, got %v", err)
			}
		})
	}

	// Invalid retry values are still caught by admission validation before Unimplemented.
	_, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "bad-retry"},
		Spec:     &v1alpha1.TaskSpec{Retry: &v1alpha1.RetryConfig{MaxRetries: -1}},
	}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("CreateTask(negative maxRetries) = %v, want InvalidArgument", err)
	}

	// When a Workspace in the store defines a profile with ateTemplate,
	// a task omitting spec.profile inherits the Workspace's template.
	if _, err := rec.Client().EnsureActorTemplateWithImage(ctx, "default", "default", "default", "ws-user-tpl", "alpine", "", nil, nil); err != nil {
		t.Fatalf("EnsureActorTemplateWithImage: %v", err)
	}
	if err := st.SaveWorkspace(ctx, &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "ws-tpl", Atespace: "default"},
		Spec:     &v1alpha1.WorkspaceSpec{Profile: &v1alpha1.Profile{AteTemplate: &v1alpha1.ObjectRef{Name: "ws-user-tpl"}}},
	}); err != nil {
		t.Fatalf("SaveWorkspace: %v", err)
	}
	inherited, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "inherits-ws-tpl"},
		Spec:     &v1alpha1.TaskSpec{Workspace: &v1alpha1.WorkspaceRef{Name: "ws-tpl"}},
	}})
	if err != nil {
		t.Fatalf("CreateTask inheriting workspace ateTemplate: %v", err)
	}
	if got := inherited.GetStatus().GetAteTemplate().GetName(); got != "ws-user-tpl" {
		t.Errorf("AteTemplate.Name = %q, want ws-user-tpl", got)
	}

	// With no default image, a task omitting spec.profile is rejected when
	// its Workspace has no profile to inherit.
	if err := st.SaveWorkspace(ctx, &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "ws-no-profile", Atespace: "default"},
		Spec:     &v1alpha1.WorkspaceSpec{},
	}); err != nil {
		t.Fatalf("SaveWorkspace: %v", err)
	}
	_, err = srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "nothing-to-inherit"},
		Spec:     &v1alpha1.TaskSpec{Workspace: &v1alpha1.WorkspaceRef{Name: "ws-no-profile"}},
	}})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "spec.profile") {
		t.Fatalf("CreateTask without a profile to inherit = %v, want InvalidArgument about spec.profile", err)
	}
	if _, err := st.GetTask(ctx, "default", "nothing-to-inherit"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected rejected task not to be saved, got %v", err)
	}

	overrideTask, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "overrides-ws-profile"},
		Spec: &v1alpha1.TaskSpec{
			Workspace: &v1alpha1.WorkspaceRef{Name: "ws-tpl"},
			Profile: &v1alpha1.Profile{
				Image: "alpine",
				Resources: &v1alpha1.ResourcesSpec{
					Limits: &v1alpha1.ResourceList{Memory: "1Gi"},
				},
			},
		},
	}})
	if err != nil {
		t.Fatalf("CreateTask with explicit profile over workspace ateTemplate.name failed: %v", err)
	}
	if got := overrideTask.GetStatus().GetAteTemplate().GetName(); !strings.HasPrefix(got, "ax-") {
		t.Errorf("AteTemplate.Name = %q, want profile template prefix ax-", got)
	}
}

func TestServer_ActorUIDIsLiveAndNotStored(t *testing.T) {
	st := memory.NewStore()
	sub := substratetest.NewServer(t)
	rec := newTestTaskManager(sub)
	srv := server.NewServer(st, rec)
	ctx := t.Context()
	// Even a client-supplied UID must not enter storage.
	if _, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "identity"},
		Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
		Status:   &v1alpha1.TaskStatus{ActorUid: "client-uid"},
	}}); err != nil {
		t.Fatal(err)
	}
	assertNotStored := func() {
		t.Helper()
		stored, err := st.GetTask(ctx, "default", "identity")
		if err != nil {
			t.Fatal(err)
		}
		if uid := stored.GetStatus().GetActorUid(); uid != "" {
			t.Fatalf("stored actor UID = %q, want empty", uid)
		}
	}
	assertNotStored()
	for _, uid := range []string{"uid-original", "uid-recreated"} {
		t.Run(uid, func(t *testing.T) {
			actor, _ := sub.Actor("default", "identity")
			actor.Metadata.Uid = uid
			sub.SetActor(actor)
			got, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "identity"})
			if err != nil {
				t.Fatal(err)
			}
			if got.GetStatus().GetActorUid() != uid {
				t.Errorf("GetTask UID = %q, want %q", got.GetStatus().GetActorUid(), uid)
			}
			list, err := srv.ListTasks(ctx, &v1alpha1.ListTasksRequest{Atespace: "default"})
			if err != nil {
				t.Fatal(err)
			}
			if len(list.Tasks) != 1 || list.Tasks[0].GetStatus().GetActorUid() != uid {
				t.Fatalf("ListTasks = %v, want one task with UID %q", list.Tasks, uid)
			}
		})
	}
	// Both mutation paths read the actor before writing the task status.
	if _, err := srv.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Name: "identity"}); err != nil {
		t.Fatal(err)
	}
	assertNotStored()
	if _, err := srv.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{Name: "identity"}); err != nil {
		t.Fatal(err)
	}
	assertNotStored()
	if err := sub.Client().DeleteActor(ctx, "default", "identity"); err != nil {
		t.Fatal(err)
	}
	got, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "identity"})
	if err != nil {
		t.Fatal(err)
	}
	if uid := got.GetStatus().GetActorUid(); uid != "" {
		t.Errorf("UID after actor deletion = %q, want empty", uid)
	}
}
