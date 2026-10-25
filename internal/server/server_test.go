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
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/ax/internal/server"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestServerHealthzHTTP(t *testing.T) {
	memStore := memory.NewStore()
	srv := server.NewServer(memStore)

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
	srv := server.NewServer(memStore)

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
	if _, err := client.UpdateWorkspace(ctx, &v1alpha1.UpdateWorkspaceRequest{Workspace: &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "grpc-ws"},
		Spec:     &v1alpha1.WorkspaceSpec{},
	}}); err != nil {
		t.Fatalf("UpdateWorkspace failed: %v", err)
	}
	if _, err := client.UpdateModel(ctx, &v1alpha1.UpdateModelRequest{Model: &v1alpha1.Model{
		Metadata: &v1alpha1.ObjectMeta{Name: "grpc-model"},
		Spec:     &v1alpha1.ModelSpec{Provider: "google", Model: "gemini-3.8-flash"},
	}}); err != nil {
		t.Fatalf("UpdateModel failed: %v", err)
	}
	if _, err := client.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "grpc-task"},
		Spec:     &v1alpha1.TaskSpec{Image: "alpine"},
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
	task.Spec.Image = "ghcr.io/test/updated-image"
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
	if suspTask.Status.Phase != "Suspended" {
		t.Errorf("expected task phase to be 'Suspended', got %q", suspTask.Status.Phase)
	}

	resTask, err := client.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Atespace: "default", Name: "grpc-task"})
	if err != nil {
		t.Fatalf("ResumeTask failed: %v", err)
	}
	if resTask.Status.Phase != "Running" {
		t.Errorf("expected task phase to be 'Running', got %q", resTask.Status.Phase)
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

	// 7b. WatchTask (should receive INITIAL state)
	stream, err := client.WatchTask(ctx, &v1alpha1.WatchTaskRequest{Atespace: "default", Name: "grpc-task"})
	if err != nil {
		t.Fatalf("WatchTask failed: %v", err)
	}
	watchMsg, err := stream.Recv()
	if err != nil {
		t.Fatalf("WatchTask Recv failed: %v", err)
	}
	if watchMsg.Action != "INITIAL" {
		t.Errorf("expected INITIAL action, got %s", watchMsg.Action)
	}
	if watchMsg.Task == nil || watchMsg.Task.Metadata == nil || watchMsg.Task.Metadata.Name != "grpc-task" {
		t.Errorf("expected task 'grpc-task' in watch, got %+v", watchMsg.Task)
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
	if _, err := client.DeleteModel(ctx, &v1alpha1.DeleteModelRequest{Atespace: "default", Name: "grpc-model"}); err != nil {
		t.Fatalf("DeleteModel failed: %v", err)
	}

	// Check 404 after delete
	_, err = client.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: "default", Name: "grpc-task"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound code, got %v", err)
	}
}

// Names and atespaces become Substrate resource names, which must be RFC 1123
// labels. The server rejects them up front instead of failing during Substrate
// actor creation.
func TestCreate_RejectsInvalidNames(t *testing.T) {
	srv := server.NewServer(memory.NewStore())
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
		if _, err := srv.UpdateWorkspace(ctx, &v1alpha1.UpdateWorkspaceRequest{Workspace: &v1alpha1.Workspace{Metadata: meta}}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("UpdateWorkspace(%v): got %v, want InvalidArgument", meta, err)
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
	if _, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{Metadata: good}}); err != nil {
		t.Errorf("CreateTask(%v): %v", good, err)
	}
	if _, err := srv.UpdateWorkspace(ctx, &v1alpha1.UpdateWorkspaceRequest{Workspace: &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "ws-1"}}}); err != nil {
		t.Errorf("UpdateWorkspace: %v", err)
	}
	if _, err := srv.UpdateModel(ctx, &v1alpha1.UpdateModelRequest{Model: &v1alpha1.Model{Metadata: &v1alpha1.ObjectMeta{Name: "gemini"}}}); err != nil {
		t.Errorf("UpdateModel: %v", err)
	}
}

func TestCreateTask_ValidatesWorkspaceBinding(t *testing.T) {
	srv := server.NewServer(memory.NewStore())
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

	_, err = srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "good"},
		Spec: &v1alpha1.TaskSpec{
			Workspace: &v1alpha1.WorkspaceRef{Name: "a"},
		},
	}})
	if err != nil {
		t.Fatalf("expected a valid workspace binding to be accepted, got %v", err)
	}
}

type fakeReconciler struct {
	reconcileCount      int
	reconcileErr        error
	reconcilePhase      string
	reconcileConditions []*v1alpha1.Condition
	observedTask        *v1alpha1.Task
	deleteCount         int
	deleteErr           error
	onDelete            func(ctx context.Context, atespace, taskName string) error
}

func (f *fakeReconciler) Reconcile(ctx context.Context, task *v1alpha1.Task, ws *v1alpha1.Workspace) (*v1alpha1.Task, error) {
	f.reconcileCount++
	f.observedTask = proto.Clone(task).(*v1alpha1.Task)
	if f.reconcileErr != nil {
		if task.Status == nil {
			task.Status = &v1alpha1.TaskStatus{}
		}
		if f.reconcilePhase != "" {
			task.Status.Phase = f.reconcilePhase
		}
		if f.reconcileConditions != nil {
			task.Status.Conditions = f.reconcileConditions
		}
		return task, f.reconcileErr
	}
	task.Status = &v1alpha1.TaskStatus{
		Phase: task.GetStatus().GetPhase(),
	}
	return task, nil
}

func (f *fakeReconciler) ReconcileDelete(ctx context.Context, atespace, taskName string) error {
	f.deleteCount++
	if f.onDelete != nil {
		return f.onDelete(ctx, atespace, taskName)
	}
	return f.deleteErr
}

func TestServer_DirectReconcilerLifecycle(t *testing.T) {
	rec := &fakeReconciler{}
	srv := server.NewServer(memory.NewStore(), server.Options{Reconciler: rec})
	ctx := context.Background()

	// 1. CreateTask directly calls Reconciler
	task, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "task-rec"},
			Spec:     &v1alpha1.TaskSpec{Image: "alpine"},
		},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if rec.reconcileCount != 1 {
		t.Errorf("expected 1 reconcile call on CreateTask, got %d", rec.reconcileCount)
	}
	if task.GetStatus().GetPhase() != "Suspended" {
		t.Errorf("expected phase Suspended, got %s", task.GetStatus().GetPhase())
	}

	// 2. ResumeTask directly calls Reconciler
	task, err = srv.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Name: "task-rec"})
	if err != nil {
		t.Fatalf("ResumeTask: %v", err)
	}
	if rec.reconcileCount != 2 {
		t.Errorf("expected 2 reconcile calls after ResumeTask, got %d", rec.reconcileCount)
	}
	if task.GetStatus().GetPhase() != "Running" {
		t.Errorf("expected phase Running, got %s", task.GetStatus().GetPhase())
	}

	// 3. SuspendTask directly calls Reconciler
	task, err = srv.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{Name: "task-rec"})
	if err != nil {
		t.Fatalf("SuspendTask: %v", err)
	}
	if rec.reconcileCount != 3 {
		t.Errorf("expected 3 reconcile calls after SuspendTask, got %d", rec.reconcileCount)
	}
	if task.GetStatus().GetPhase() != "Suspended" {
		t.Errorf("expected phase Suspended, got %s", task.GetStatus().GetPhase())
	}

	// 4. DeleteTask directly calls ReconcileDelete
	_, err = srv.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Name: "task-rec"})
	if err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	if rec.deleteCount != 1 {
		t.Errorf("expected 1 delete call, got %d", rec.deleteCount)
	}

	// 5. Verify task is gone
	_, err = srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "task-rec"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound after DeleteTask, got %v", err)
	}
}

func TestServer_DeleteTask_BlocksAndSetsPhaseTerminating(t *testing.T) {
	st := memory.NewStore()
	var observedPhaseDuringDelete string
	rec := &fakeReconciler{
		onDelete: func(ctx context.Context, atespace, taskName string) error {
			t, err := st.GetTask(ctx, atespace, taskName)
			if err == nil && t.Status != nil {
				observedPhaseDuringDelete = t.Status.Phase
			}
			return nil
		},
	}
	srv := server.NewServer(st, server.Options{Reconciler: rec})
	ctx := context.Background()

	_, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "task-term"},
			Spec:     &v1alpha1.TaskSpec{Image: "alpine"},
		},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	_, err = srv.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Name: "task-term"})
	if err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}

	if observedPhaseDuringDelete != v1alpha1.PhaseTerminating {
		t.Errorf("expected phase %q during delete, got %q", v1alpha1.PhaseTerminating, observedPhaseDuringDelete)
	}

	_, err = srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "task-term"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound after successful DeleteTask, got %v", err)
	}
}

func TestServer_DeleteTask_ReconcileErrorRetainsTask(t *testing.T) {
	st := memory.NewStore()
	rec := &fakeReconciler{
		deleteErr: errors.New("substrate timeout deleting actor"),
	}
	srv := server.NewServer(st, server.Options{Reconciler: rec})
	ctx := context.Background()

	_, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "task-err"},
			Spec:     &v1alpha1.TaskSpec{Image: "alpine"},
		},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	_, err = srv.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Name: "task-err"})
	if err == nil {
		t.Fatal("expected DeleteTask to fail when ReconcileDelete fails")
	}

	task, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "task-err"})
	if err != nil {
		t.Fatalf("expected task to remain in store, got %v", err)
	}
	if task.GetStatus().GetPhase() != v1alpha1.PhaseTerminating {
		t.Errorf("expected phase %q, got %q", v1alpha1.PhaseTerminating, task.GetStatus().GetPhase())
	}
}

type failUpdateStatusStore struct {
	store.Store
	failUpdateStatus bool
}

func (f *failUpdateStatusStore) UpdateTaskStatus(ctx context.Context, atespace, name string, status *v1alpha1.TaskStatus) error {
	if f.failUpdateStatus {
		return errors.New("simulated store failure")
	}
	return f.Store.UpdateTaskStatus(ctx, atespace, name, status)
}

func TestServer_DeleteTask_UpdateStatusError(t *testing.T) {
	base := memory.NewStore()
	st := &failUpdateStatusStore{Store: base}
	rec := &fakeReconciler{}
	srv := server.NewServer(st, server.Options{Reconciler: rec})
	ctx := context.Background()

	_, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "task-status-fail"},
			Spec:     &v1alpha1.TaskSpec{Image: "alpine"},
		},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	st.failUpdateStatus = true
	_, err = srv.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Name: "task-status-fail"})
	if err == nil {
		t.Fatal("expected DeleteTask to fail when UpdateTaskStatus fails")
	}
	if rec.deleteCount != 0 {
		t.Errorf("expected ReconcileDelete not to be called if UpdateTaskStatus fails, got %d calls", rec.deleteCount)
	}
}

func TestServer_SuspendTask_ReconcileErrorRetainsRunningState(t *testing.T) {
	st := memory.NewStore()
	rec := &fakeReconciler{
		reconcileErr: errors.New("substrate unavailable"),
	}
	srv := server.NewServer(st, server.Options{Reconciler: rec})
	ctx := context.Background()

	if err := st.SaveTask(ctx, &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "task-suspend-err", Atespace: "default"},
		Spec:     &v1alpha1.TaskSpec{Image: "alpine"},
		Status: &v1alpha1.TaskStatus{
			Phase:      "Running",
			Actor:      "task-suspend-err",
			WorkerIp:   "10.244.1.42",
			Conditions: []*v1alpha1.Condition{{Type: "Ready", Status: "True", Reason: v1alpha1.ReasonTaskRunning}},
		},
	}); err != nil {
		t.Fatalf("SaveTask: %v", err)
	}

	_, err := srv.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{Name: "task-suspend-err"})
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal error when Reconcile fails on SuspendTask, got %v", err)
	}

	stored, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "task-suspend-err"})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.GetStatus().GetPhase() != "Running" {
		t.Errorf("stored phase = %q, want Running after failed suspension", stored.GetStatus().GetPhase())
	}
	if stored.GetStatus().GetWorkerIp() != "10.244.1.42" {
		t.Errorf("stored worker IP = %q, want 10.244.1.42", stored.GetStatus().GetWorkerIp())
	}
	if len(stored.GetStatus().GetConditions()) != 1 || stored.GetStatus().GetConditions()[0].GetReason() != v1alpha1.ReasonTaskRunning {
		t.Errorf("stored conditions = %v, want Ready=True (TaskRunning)", stored.GetStatus().GetConditions())
	}
}

func TestServer_SuspendTask_SuccessUpdatesStoredState(t *testing.T) {
	st := memory.NewStore()
	rec := &fakeReconciler{}
	srv := server.NewServer(st, server.Options{Reconciler: rec})
	ctx := context.Background()

	if err := st.SaveTask(ctx, &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "task-suspend-ok", Atespace: "default"},
		Spec:     &v1alpha1.TaskSpec{Image: "alpine"},
		Status: &v1alpha1.TaskStatus{
			Phase:      "Running",
			Actor:      "task-suspend-ok",
			WorkerIp:   "10.244.1.42",
			Conditions: []*v1alpha1.Condition{{Type: "Ready", Status: "True", Reason: v1alpha1.ReasonTaskRunning}},
		},
	}); err != nil {
		t.Fatalf("SaveTask: %v", err)
	}

	resp, err := srv.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{Name: "task-suspend-ok"})
	if err != nil {
		t.Fatalf("SuspendTask: %v", err)
	}
	if resp.GetStatus().GetPhase() != "Suspended" {
		t.Errorf("response phase = %q, want Suspended", resp.GetStatus().GetPhase())
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
	if st.GetPhase() != "" || st.GetActor() != "" || st.GetWorkerIp() != "" {
		t.Errorf("stored status has phase=%q actor=%q workerIP=%q, want all empty", st.GetPhase(), st.GetActor(), st.GetWorkerIp())
	}
	for _, c := range st.GetConditions() {
		if c.GetType() == "Ready" {
			t.Errorf("stored status has a Ready condition %v, want none", c)
		}
	}
}

// fakeSubstrate stands in for Substrate's control API: every actor named in
// names exists in the default atespace, in the given state.
type fakeSubstrate struct {
	ateapipb.UnimplementedControlServer
	mu       sync.Mutex
	names    []string
	state    ateapipb.ActorState
	workerIP string
}

func (f *fakeSubstrate) setState(state ateapipb.ActorState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = state
}

func (f *fakeSubstrate) actor(name string) *ateapipb.Actor {
	a := &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "default", Name: name},
		Status:   &ateapipb.ActorStatus{State: f.state},
	}
	if f.workerIP != "" {
		a.Status.WorkerAssignment = &ateapipb.WorkerAssignment{WorkerPodIps: []string{f.workerIP}}
	}
	return a
}

func (f *fakeSubstrate) GetActor(_ context.Context, req *ateapipb.GetActorRequest) (*ateapipb.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, name := range f.names {
		if req.GetActor().GetAtespace() == "default" && req.GetActor().GetName() == name {
			return f.actor(name), nil
		}
	}
	return nil, status.Error(codes.NotFound, "actor not found")
}

func (f *fakeSubstrate) ListActors(_ context.Context, req *ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := &ateapipb.ListActorsResponse{}
	if req.GetAtespace() == "default" {
		for _, name := range f.names {
			resp.Actors = append(resp.Actors, f.actor(name))
		}
	}
	return resp, nil
}

// client serves f and returns a Substrate client connected to it.
func (f *fakeSubstrate) client(t *testing.T) *substrate.Client {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	g := grpc.NewServer()
	ateapipb.RegisterControlServer(g, f)
	go g.Serve(lis)
	t.Cleanup(g.Stop)
	c, err := substrate.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("substrate client: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestServer_ReadsActorStateFromSubstrate(t *testing.T) {
	st := memory.NewStore()
	sub := &fakeSubstrate{names: []string{"observed-a", "observed-b"}, state: ateapipb.ActorState_ACTOR_STATE_RUNNING, workerIP: "10.244.1.7"}
	srv := server.NewServer(st, server.Options{Reconciler: &fakeReconciler{}, Substrate: sub.client(t)})
	ctx := context.Background()

	for _, name := range []string{"observed-a", "observed-b"} {
		if _, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: name},
			Spec:     &v1alpha1.TaskSpec{Image: "alpine"},
		}}); err != nil {
			t.Fatalf("CreateTask(%s): %v", name, err)
		}
	}

	got, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "observed-a"})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.GetStatus().GetPhase() != "Running" || got.GetStatus().GetWorkerIp() != "10.244.1.7" || got.GetStatus().GetActor() != "observed-a" {
		t.Errorf("GetTask status = %v, want the actor's state", got.GetStatus())
	}

	list, err := srv.ListTasks(ctx, &v1alpha1.ListTasksRequest{Atespace: "default"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	for _, task := range list.GetTasks() {
		if task.GetStatus().GetPhase() != "Running" || task.GetStatus().GetActor() != task.GetMetadata().GetName() {
			t.Errorf("ListTasks status for %s = %v, want the actor's state", task.GetMetadata().GetName(), task.GetStatus())
		}
	}

	// A change on Substrate alone is visible on the next read.
	sub.setState(ateapipb.ActorState_ACTOR_STATE_SUSPENDED)
	got, err = srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "observed-a"})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.GetStatus().GetPhase() != "Suspended" {
		t.Errorf("phase after the actor was suspended on Substrate = %q, want Suspended", got.GetStatus().GetPhase())
	}

	stored, err := st.GetTask(ctx, "default", "observed-a")
	if err != nil {
		t.Fatalf("store GetTask: %v", err)
	}
	assertNoActorState(t, stored)
}

func TestServer_CreateTask_InitialPhaseSuspended(t *testing.T) {
	st := memory.NewStore()
	rec := &fakeReconciler{}
	srv := server.NewServer(st, server.Options{Reconciler: rec})
	ctx := context.Background()

	task, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "task-create-suspended"},
			Spec:     &v1alpha1.TaskSpec{Image: "alpine"},
		},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.GetStatus().GetPhase() != "Suspended" {
		t.Errorf("returned task phase = %q, want Suspended", task.GetStatus().GetPhase())
	}

	stored, err := st.GetTask(ctx, "default", "task-create-suspended")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	assertNoActorState(t, stored)
}

func TestServer_CreateTask_ReconcileErrorMarksFailed(t *testing.T) {
	st := memory.NewStore()
	rec := &fakeReconciler{
		reconcileErr: errors.New("substrate suspend actor failure"),
	}
	srv := server.NewServer(st, server.Options{Reconciler: rec})
	ctx := context.Background()

	_, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "task-create-fail"},
			Spec:     &v1alpha1.TaskSpec{Image: "alpine"},
		},
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal error when Reconcile fails on CreateTask, got %v", err)
	}

	// Task was saved and updated to Failed phase in store
	stored, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "task-create-fail"})
	if err != nil {
		t.Fatalf("expected task to exist in store after reconcile error, got %v", err)
	}
	if stored.GetStatus().GetPhase() != v1alpha1.PhaseFailed {
		t.Errorf("stored phase = %q, want Failed", stored.GetStatus().GetPhase())
	}

	// Because task already exists in store, attempting to create it again must fail with FailedPrecondition
	_, err = srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "task-create-fail"},
			Spec:     &v1alpha1.TaskSpec{Image: "alpine"},
		},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition on duplicate create, got %v", err)
	}
}

func TestServer_TransientSubstrateUnavailableRecovery(t *testing.T) {
	st := memory.NewStore()
	rec := &fakeReconciler{
		reconcileErr:   status.Error(codes.Unavailable, "connection refused"),
		reconcilePhase: v1alpha1.PhasePending,
		reconcileConditions: []*v1alpha1.Condition{
			{Type: "Ready", Status: "False", Reason: v1alpha1.ReasonSubstrateUnavailable},
		},
	}
	srv := server.NewServer(st, server.Options{Reconciler: rec})
	ctx := context.Background()

	// 1. CreateTask while Substrate is unavailable records Phase="Pending" (not "Failed")
	// and returns codes.Unavailable.
	_, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "simple-task"},
			Spec:     &v1alpha1.TaskSpec{Image: "alpine"},
		},
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable from CreateTask when Substrate is unreachable, got %v", err)
	}

	stored, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "simple-task"})
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if stored.GetStatus().GetPhase() != v1alpha1.PhasePending {
		t.Errorf("expected stored phase 'Pending' after transient Substrate error, got %q", stored.GetStatus().GetPhase())
	}

	// 2. Re-applying a Pending task with a different spec is rejected to preserve immutability.
	_, err = srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "simple-task"},
			Spec:     &v1alpha1.TaskSpec{Image: "debian"},
		},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition when re-applying Pending task with modified spec, got %v", err)
	}

	// 3. Re-applying a Pending task with omitted status preserves stored status conditions
	// rather than dropping them before reconciliation.
	rec.reconcileConditions = nil
	_, err = srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "simple-task"},
			Spec:     &v1alpha1.TaskSpec{Image: "alpine"},
		},
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable on retry while Substrate is still down, got %v", err)
	}
	if conds := rec.observedTask.GetStatus().GetConditions(); len(conds) != 1 || conds[0].GetReason() != v1alpha1.ReasonSubstrateUnavailable {
		t.Errorf("expected stored condition SubstrateUnavailable to be preserved on omitted-status retry, got %v", conds)
	}

	// 4. Re-applying a Pending task with client-supplied conditions reuses the stored task
	// rather than overwriting status with client-supplied values.
	_, err = srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "simple-task"},
			Spec:     &v1alpha1.TaskSpec{Image: "alpine"},
			Status: &v1alpha1.TaskStatus{
				Phase: "Running",
				Conditions: []*v1alpha1.Condition{
					{Type: "Ready", Status: "True", Reason: "ClientForged"},
				},
			},
		},
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable on retry while Substrate is still down, got %v", err)
	}
	if conds := rec.observedTask.GetStatus().GetConditions(); len(conds) != 1 || conds[0].GetReason() != v1alpha1.ReasonSubstrateUnavailable {
		t.Errorf("expected client-supplied conditions to be ignored on Pending retry, got %v", conds)
	}

	// 5. Once Substrate recovers, re-applying CreateTask with the unchanged spec succeeds
	// for a Pending task and transitions it to Suspended.
	rec.reconcileErr = nil
	rec.reconcilePhase = ""
	created, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "simple-task"},
			Spec:     &v1alpha1.TaskSpec{Image: "alpine"},
		},
	})
	if err != nil {
		t.Fatalf("CreateTask retry after Substrate recovery failed: %v", err)
	}
	if created.GetStatus().GetPhase() != "Suspended" {
		t.Errorf("expected phase 'Suspended' after recovery, got %q", created.GetStatus().GetPhase())
	}
	if !created.GetMetadata().GetCreateTime().AsTime().Equal(stored.GetMetadata().GetCreateTime().AsTime()) {
		t.Errorf("expected CreateTime to be preserved across Pending re-apply")
	}

	// 6. ResumeTask during a brief Substrate outage keeps the task Pending instead of Failed.
	rec.reconcileErr = status.Error(codes.Unavailable, "substrate restarting")
	rec.reconcilePhase = v1alpha1.PhasePending
	_, err = srv.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Name: "simple-task"})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable from ResumeTask when Substrate is unreachable, got %v", err)
	}
	storedAfterResumeErr, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: "simple-task"})
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if storedAfterResumeErr.GetStatus().GetPhase() != v1alpha1.PhasePending {
		t.Errorf("expected stored phase 'Pending' after transient ResumeTask error, got %q", storedAfterResumeErr.GetStatus().GetPhase())
	}
}

func TestServer_WatchTaskSeesSubstrateOnlyChanges(t *testing.T) {
	defer server.SetWatchPollInterval(10 * time.Millisecond)()

	st := memory.NewStore()
	sub := &fakeSubstrate{names: []string{"watched"}, state: ateapipb.ActorState_ACTOR_STATE_SUSPENDED}
	srv := server.NewServer(st, server.Options{Reconciler: &fakeReconciler{}, Substrate: sub.client(t)})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "watched"},
		Spec:     &v1alpha1.TaskSpec{Image: "alpine"},
	}}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.GRPCServer().Serve(ln)
	defer srv.GRPCServer().Stop()
	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	stream, err := v1alpha1.NewAXClient(conn).WatchTask(ctx, &v1alpha1.WatchTaskRequest{Name: "watched"})
	if err != nil {
		t.Fatalf("WatchTask: %v", err)
	}
	evt, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if evt.GetAction() != "INITIAL" || evt.GetTask().GetStatus().GetPhase() != "Suspended" {
		t.Fatalf("first event = %s/%s, want INITIAL/Suspended", evt.GetAction(), evt.GetTask().GetStatus().GetPhase())
	}

	// The actor crashes on Substrate. AX writes nothing, but the watcher hears about it.
	sub.setState(ateapipb.ActorState_ACTOR_STATE_CRASHED)
	evt, err = stream.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if evt.GetAction() != "MODIFIED" || evt.GetTask().GetStatus().GetPhase() != v1alpha1.PhaseFailed {
		t.Fatalf("second event = %s/%s, want MODIFIED/Failed", evt.GetAction(), evt.GetTask().GetStatus().GetPhase())
	}
}
