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
	"net/http"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/ax/internal/controller"
	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type mockControlServer struct {
	ateapipb.UnimplementedControlServer
	workerIP         string
	createdAtespaces []string
	createdActors    []string
	resumedActors    []string
	suspendedActors  []string
	deletedActors    []string
	actorTemplates   map[string]bool
	createdTemplates []*ateapipb.ActorTemplate
	deletedTemplates []string
	// createTemplateErr, when set, is returned by CreateActorTemplate to stand in
	// for Substrate rejecting a template (for example an invalid quantity).
	createTemplateErr error
}

// noSecrets is a SecretResolver for tests: it never finds a key and never touches a cluster.
func noSecrets(context.Context, string, string, string) (string, error) {
	return "", nil
}

func (m *mockControlServer) GetActorTemplate(_ context.Context, req *ateapipb.GetActorTemplateRequest) (*ateapipb.ActorTemplate, error) {
	ref := req.GetActorTemplate()
	if !m.actorTemplates[ref.GetName()] {
		return nil, status.Error(codes.NotFound, "template not found")
	}
	return &ateapipb.ActorTemplate{Metadata: &ateapipb.ResourceMetadata{Name: ref.GetName(), Atespace: ref.GetAtespace()}}, nil
}

func (m *mockControlServer) CreateActorTemplate(_ context.Context, req *ateapipb.CreateActorTemplateRequest) (*ateapipb.ActorTemplate, error) {
	if m.createTemplateErr != nil {
		return nil, m.createTemplateErr
	}
	if m.actorTemplates == nil {
		m.actorTemplates = make(map[string]bool)
	}
	tmpl := req.GetActorTemplate()
	m.actorTemplates[tmpl.GetMetadata().GetName()] = true
	m.createdTemplates = append(m.createdTemplates, tmpl)
	return tmpl, nil
}

func (m *mockControlServer) CreateAtespace(ctx context.Context, req *ateapipb.CreateAtespaceRequest) (*ateapipb.Atespace, error) {
	name := ""
	if req.Atespace != nil && req.Atespace.Metadata != nil {
		name = req.Atespace.Metadata.Name
	}
	m.createdAtespaces = append(m.createdAtespaces, name)
	return &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: name}}, nil
}

func (m *mockControlServer) CreateActor(ctx context.Context, req *ateapipb.CreateActorRequest) (*ateapipb.Actor, error) {
	name := ""
	if req.Actor != nil && req.Actor.Metadata != nil {
		name = req.Actor.Metadata.Name
	}
	m.createdActors = append(m.createdActors, name)
	return &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Name: name},
		Status: &ateapipb.ActorStatus{
			State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
		},
	}, nil
}

func (m *mockControlServer) ResumeActor(ctx context.Context, req *ateapipb.ResumeActorRequest) (*ateapipb.ResumeActorResponse, error) {
	name := ""
	if req.Actor != nil {
		name = req.Actor.Name
	}
	m.resumedActors = append(m.resumedActors, name)
	wIP := "10.244.1.42"
	if m.workerIP != "" {
		wIP = m.workerIP
	}
	return &ateapipb.ResumeActorResponse{
		Actor: &ateapipb.Actor{
			Metadata: &ateapipb.ResourceMetadata{Name: name},
			Status: &ateapipb.ActorStatus{
				State: ateapipb.ActorState_ACTOR_STATE_RUNNING,
				WorkerAssignment: &ateapipb.WorkerAssignment{
					WorkerPod:   "worker-pod-1",
					WorkerPodIp: wIP,
				},
			},
		},
		Resumed: true,
	}, nil
}

func (m *mockControlServer) SuspendActor(ctx context.Context, req *ateapipb.SuspendActorRequest) (*ateapipb.SuspendActorResponse, error) {
	name := ""
	if req.Actor != nil {
		name = req.Actor.Name
	}
	m.suspendedActors = append(m.suspendedActors, name)
	return &ateapipb.SuspendActorResponse{}, nil
}


func (m *mockControlServer) DeleteActor(ctx context.Context, req *ateapipb.DeleteActorRequest) (*ateapipb.Actor, error) {
	name := req.GetActor().GetName()
	m.deletedActors = append(m.deletedActors, name)
	return &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Name: name}}, nil
}

func (m *mockControlServer) ListActorTemplates(ctx context.Context, req *ateapipb.ListActorTemplatesRequest) (*ateapipb.ListActorTemplatesResponse, error) {
	resp := &ateapipb.ListActorTemplatesResponse{}
	for name := range m.actorTemplates {
		resp.ActorTemplates = append(resp.ActorTemplates, &ateapipb.ActorTemplate{
			Metadata: &ateapipb.ResourceMetadata{Name: name, Atespace: req.GetAtespace()},
		})
	}
	return resp, nil
}

func (m *mockControlServer) DeleteActorTemplate(ctx context.Context, req *ateapipb.DeleteActorTemplateRequest) (*ateapipb.ActorTemplate, error) {
	name := req.GetActorTemplate().GetName()
	delete(m.actorTemplates, name)
	m.deletedTemplates = append(m.deletedTemplates, name)
	return &ateapipb.ActorTemplate{Metadata: &ateapipb.ResourceMetadata{Name: name}}, nil
}

func TestTaskReconciler(t *testing.T) {
	ctx := context.Background()

	// 1. Start in-process mock gRPC Substrate server
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	mockSrv := &mockControlServer{}
	grpcServer := grpc.NewServer()
	ateapipb.RegisterControlServer(grpcServer, mockSrv)
	go grpcServer.Serve(lis)
	defer grpcServer.Stop()

	// 2. Initialize Substrate client
	client, err := substrate.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	// 3. Reconcile Task
	reconciler := controller.NewTaskReconciler(client, "test-template", "ax-system")
	reconciler.SecretResolver = noSecrets
	reconciler.WorkspaceReadyTimeout = 200 * time.Millisecond

	task := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata: &v1alpha1.ObjectMeta{
			Name:     "test-task",
			Atespace: "default",
		},
		Spec: &v1alpha1.TaskSpec{
			Image:   "ghrc.io/my-org/my-image",
			Command: []string{"/bin/task-runner"},
		},
		// A client-supplied actor name must not survive: the actor is always
		// named after the task.
		Status: &v1alpha1.TaskStatus{Actor: "not-the-task", Phase: "Running"},
	}

	reconciled, err := reconciler.Reconcile(ctx, task)
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// 4. Validate reconciliation results
	if reconciled.Status.Phase != "Running" {
		t.Errorf("expected phase 'Running', got %q", reconciled.Status.Phase)
	}
	if reconciled.Status.Actor != "test-task" {
		t.Errorf("expected actor 'test-task', got %q", reconciled.Status.Actor)
	}
	if reconciled.Status.WorkerIp != "10.244.1.42" {
		t.Errorf("expected worker IP '10.244.1.42', got %q", reconciled.Status.WorkerIp)
	}

	// Verify mock was called
	if len(mockSrv.createdAtespaces) != 1 || mockSrv.createdAtespaces[0] != "default" {
		t.Errorf("expected atespace 'default' created, got %v", mockSrv.createdAtespaces)
	}
	if len(mockSrv.createdActors) != 1 || mockSrv.createdActors[0] != "test-task" {
		t.Errorf("expected actor 'test-task' created, got %v", mockSrv.createdActors)
	}
	if len(mockSrv.resumedActors) != 1 || mockSrv.resumedActors[0] != "test-task" {
		t.Errorf("expected actor 'test-task' resumed, got %v", mockSrv.resumedActors)
	}
}

func TestTaskReconciler_Suspend(t *testing.T) {
	ctx := context.Background()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	mockSrv := &mockControlServer{}
	grpcServer := grpc.NewServer()
	ateapipb.RegisterControlServer(grpcServer, mockSrv)
	go grpcServer.Serve(lis)
	defer grpcServer.Stop()

	client, err := substrate.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	reconciler := controller.NewTaskReconciler(client, "test-template", "ax-system")
	reconciler.SecretResolver = noSecrets
	reconciler.WorkspaceReadyTimeout = 200 * time.Millisecond

	task := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata: &v1alpha1.ObjectMeta{
			Name:     "suspend-task",
			Atespace: "default",
		},
		Spec: &v1alpha1.TaskSpec{
			Image: "ghrc.io/my-org/my-image",
		},
	}

	reconciled, err := reconciler.Reconcile(ctx, task, nil)
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	if reconciled.Status.Phase != "Suspended" {
		t.Errorf("expected phase 'Suspended', got %q", reconciled.Status.Phase)
	}
	if reconciled.Status.WorkerIp != "" {
		t.Errorf("expected empty worker IP, got %q", reconciled.Status.WorkerIp)
	}
	if len(mockSrv.suspendedActors) != 1 || mockSrv.suspendedActors[0] != "suspend-task" {
		t.Errorf("expected actor 'suspend-task' suspended, got %v", mockSrv.suspendedActors)
	}
}

func TestTaskReconciler_WorkspaceReady(t *testing.T) {
	ctx := context.Background()

	// 1. Mock Substrate Control Server
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	// 2. Mock Worker readyz HTTP server
	httpLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen http: %v", err)
	}
	defer httpLis.Close()

	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	httpServer := &http.Server{Handler: httpMux}
	go httpServer.Serve(httpLis)
	defer httpServer.Close()

	workerHost, workerPortStr, _ := net.SplitHostPort(httpLis.Addr().String())
	// In our mock, the worker IP returned by ResumeActor will have our mock ready server listening.
	// But our reconciler connects to port 9999 by default: fmt.Sprintf("http://%s:9999/readyz", workerIP).
	// If workerIP includes a port or is a host, let's verify how it handles it.
	_ = workerHost
	_ = workerPortStr

	mockSrv := &mockControlServer{}
	grpcServer := grpc.NewServer()
	ateapipb.RegisterControlServer(grpcServer, mockSrv)
	go grpcServer.Serve(lis)
	defer grpcServer.Stop()

	client, err := substrate.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	reconciler := controller.NewTaskReconciler(client, "test-template", "ax-system")
	reconciler.SecretResolver = noSecrets
	reconciler.WorkspaceReadyTimeout = 200 * time.Millisecond

	task := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata: &v1alpha1.ObjectMeta{
			Name:     "ready-task",
			Atespace: "default",
		},
		Spec: &v1alpha1.TaskSpec{},
		Status: &v1alpha1.TaskStatus{
			Phase: "Running",
		},
	}

	// Case 1: Worker not responding on readyz -> WorkspaceReady=False and Ready=False.
	reconciled, err := reconciler.Reconcile(ctx, task, nil)
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	assertCondition(t, reconciled, "WorkspaceReady", "False", "Initializing")
	assertCondition(t, reconciled, "Ready", "False", "WorkspaceInitializing")

	// Case 2: Worker readyz endpoint succeeds -> WorkspaceReady=True and Ready=True.
	mockSrv.workerIP = httpLis.Addr().String()
	reconciledReady, err := reconciler.Reconcile(ctx, task, nil)
	if err != nil {
		t.Fatalf("Reconcile with ready worker failed: %v", err)
	}
	assertCondition(t, reconciledReady, "WorkspaceReady", "True", "SetupComplete")
	assertCondition(t, reconciledReady, "Ready", "True", "TaskRunning")

	// Case 3: Suspending the task -> Ready=False (TaskSuspended), but the workspace was
	// already initialized so WorkspaceReady stays True.
	task = reconciledReady
	task.Status.Phase = "Suspended"
	reconciledSuspended, err := reconciler.Reconcile(ctx, task, nil)
	if err != nil {
		t.Fatalf("Reconcile with suspend failed: %v", err)
	}
	if reconciledSuspended.Status.Phase != "Suspended" {
		t.Errorf("expected phase Suspended, got %s", reconciledSuspended.Status.Phase)
	}
	assertCondition(t, reconciledSuspended, "Ready", "False", "TaskSuspended")
	assertCondition(t, reconciledSuspended, "WorkspaceReady", "True", "SetupComplete")

	// Case 4: Resuming with the worker unreachable -> the reconciler trusts the recorded
	// WorkspaceReady instead of re-polling, so the task is Ready again immediately.
	mockSrv.workerIP = "127.0.0.1:1"
	task = reconciledSuspended
	task.Status.Phase = "Running"
	reconciledResumed, err := reconciler.Reconcile(ctx, task, nil)
	if err != nil {
		t.Fatalf("Reconcile with resume failed: %v", err)
	}
	assertCondition(t, reconciledResumed, "WorkspaceReady", "True", "SetupComplete")
	assertCondition(t, reconciledResumed, "Ready", "True", "TaskRunning")
	if got := len(mockSrv.actorTemplates); got != 1 {
		t.Fatalf("readiness updates and suspend/resume created %d templates, want 1", got)
	}

	// A launch configuration change must still get a distinct template.
	task.Spec.Command = []string{"python3", "agent.py"}
	if _, err := reconciler.Reconcile(ctx, task, nil); err != nil {
		t.Fatalf("Reconcile with changed command failed: %v", err)
	}
	if got := len(mockSrv.actorTemplates); got != 2 {
		t.Errorf("command change left %d templates, want 2", got)
	}
}

func TestTaskReconciler_ResourceLimits(t *testing.T) {
	ctx := context.Background()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	mockSrv := &mockControlServer{}
	grpcServer := grpc.NewServer()
	ateapipb.RegisterControlServer(grpcServer, mockSrv)
	go grpcServer.Serve(lis)
	defer grpcServer.Stop()

	client, err := substrate.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	reconciler := controller.NewTaskReconciler(client, "test-template", "ax-system")
	reconciler.SecretResolver = noSecrets
	reconciler.WorkspaceReadyTimeout = 200 * time.Millisecond

	task := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata: &v1alpha1.ObjectMeta{
			Name:     "sized-task",
			Atespace: "default",
		},
		Spec: &v1alpha1.TaskSpec{
			Image: "ghrc.io/my-org/my-image",
			Resources: &v1alpha1.ResourceReqs{
				Limits: &v1alpha1.ResourceList{Cpu: "2", Memory: "4Gi"},
			},
		},
	}

	if _, err := reconciler.Reconcile(ctx, task); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	if len(mockSrv.createdTemplates) != 1 {
		t.Fatalf("created %d templates, want 1", len(mockSrv.createdTemplates))
	}
	want := &ateapipb.Resources{Limits: []*ateapipb.Limits{
		{Name: "cpu", Quantity: "2"},
		{Name: "memory", Quantity: "4Gi"},
	}}
	if got := mockSrv.createdTemplates[0].GetResources(); !proto.Equal(got, want) {
		t.Errorf("template resources = %v, want %v", got, want)
	}

	// Raising a limit is a launch configuration change and must provision a new
	// template, under a new name, carrying the new value.
	task.Spec.Resources.Limits.Memory = "8Gi"
	if _, err := reconciler.Reconcile(ctx, task); err != nil {
		t.Fatalf("Reconcile with changed limits failed: %v", err)
	}
	if len(mockSrv.createdTemplates) != 2 {
		t.Fatalf("limits change left %d templates, want 2", len(mockSrv.createdTemplates))
	}
	first, second := mockSrv.createdTemplates[0].GetMetadata().GetName(), mockSrv.createdTemplates[1].GetMetadata().GetName()
	if first == second {
		t.Errorf("limits change reused template name %q, want a distinct name", first)
	}
	want.Limits[1].Quantity = "8Gi"
	if got := mockSrv.createdTemplates[1].GetResources(); !proto.Equal(got, want) {
		t.Errorf("template resources after change = %v, want %v", got, want)
	}

	// A task without limits inherits the worker defaults: no resources block.
	plain := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata:   &v1alpha1.ObjectMeta{Name: "plain-task", Atespace: "default"},
		Spec:       &v1alpha1.TaskSpec{Image: "ghrc.io/my-org/my-image"},
	}
	if _, err := reconciler.Reconcile(ctx, plain); err != nil {
		t.Fatalf("Reconcile of task without limits failed: %v", err)
	}
	if got := mockSrv.createdTemplates[len(mockSrv.createdTemplates)-1].GetResources(); got != nil {
		t.Errorf("template for task without limits has resources %v, want none", got)
	}
}

// A task that asks for limits must never run without them: invalid limits and a
// Substrate rejection of the template both fail the reconcile instead of falling
// back to the default template. Without limits the fallback contract is unchanged.
func TestTaskReconciler_ResourceLimitsFailurePaths(t *testing.T) {
	ctx := context.Background()

	newTask := func(name string, resources *v1alpha1.ResourceReqs) *v1alpha1.Task {
		return &v1alpha1.Task{
			ApiVersion: v1alpha1.APIVersion,
			Kind:       v1alpha1.KindTask,
			Metadata:   &v1alpha1.ObjectMeta{Name: name, Atespace: "default"},
			Spec:       &v1alpha1.TaskSpec{Image: "ghrc.io/my-org/my-image", Resources: resources},
		}
	}
	setup := func(t *testing.T, mockSrv *mockControlServer) *controller.TaskReconciler {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("failed to listen: %v", err)
		}
		grpcServer := grpc.NewServer()
		ateapipb.RegisterControlServer(grpcServer, mockSrv)
		go grpcServer.Serve(lis)
		client, err := substrate.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatalf("failed to create substrate client: %v", err)
		}
		t.Cleanup(func() {
			client.Close()
			grpcServer.Stop()
			lis.Close()
		})
		reconciler := controller.NewTaskReconciler(client, "test-template", "ax-system")
		reconciler.SecretResolver = noSecrets
		reconciler.WorkspaceReadyTimeout = 200 * time.Millisecond
		return reconciler
	}

	t.Run("invalid limits fail before provisioning", func(t *testing.T) {
		mockSrv := &mockControlServer{}
		reconciler := setup(t, mockSrv)

		for _, reqs := range []*v1alpha1.ResourceReqs{
			{Limits: &v1alpha1.ResourceList{Cpu: "two"}},
			{Limits: &v1alpha1.ResourceList{Cpu: "0"}},
			{Limits: &v1alpha1.ResourceList{Cpu: "1000"}},
			{Limits: &v1alpha1.ResourceList{Memory: "-4Gi"}},
			{Requests: &v1alpha1.ResourceList{Cpu: "500m"}, Limits: &v1alpha1.ResourceList{Cpu: "2"}},
		} {
			reconciled, err := reconciler.Reconcile(ctx, newTask("bad-limits", reqs))
			if err == nil {
				t.Errorf("Reconcile(%v) succeeded, want error", reqs)
			}
			if reconciled.Status.Phase != "Failed" {
				t.Errorf("Reconcile(%v) phase = %q, want Failed", reqs, reconciled.Status.Phase)
			}
			assertCondition(t, reconciled, "Ready", "False", "InvalidResources")
		}
		if len(mockSrv.createdTemplates) != 0 || len(mockSrv.createdActors) != 0 || len(mockSrv.resumedActors) != 0 {
			t.Errorf("invalid limits reached Substrate: templates=%d actors=%v resumed=%v",
				len(mockSrv.createdTemplates), mockSrv.createdActors, mockSrv.resumedActors)
		}
	})

	t.Run("rejected template with limits does not fall back", func(t *testing.T) {
		mockSrv := &mockControlServer{
			createTemplateErr: status.Error(codes.InvalidArgument, "actor_template.resources.limits[0].quantity: Invalid value"),
		}
		reconciler := setup(t, mockSrv)

		reconciled, err := reconciler.Reconcile(ctx, newTask("rejected-limits", &v1alpha1.ResourceReqs{
			Limits: &v1alpha1.ResourceList{Cpu: "2", Memory: "4Gi"},
		}))
		if err == nil {
			t.Fatal("Reconcile succeeded although the template with limits was rejected")
		}
		if reconciled.Status.Phase != "Failed" {
			t.Errorf("phase = %q, want Failed", reconciled.Status.Phase)
		}
		assertCondition(t, reconciled, "Ready", "False", "TemplateCreationFailed")
		if len(mockSrv.createdActors) != 0 || len(mockSrv.resumedActors) != 0 {
			t.Errorf("task ran on a fallback template without its limits: actors=%v resumed=%v", mockSrv.createdActors, mockSrv.resumedActors)
		}
	})

	t.Run("rejected template without limits still falls back", func(t *testing.T) {
		mockSrv := &mockControlServer{
			createTemplateErr: status.Error(codes.InvalidArgument, "image must be pinned by digest"),
		}
		reconciler := setup(t, mockSrv)

		reconciled, err := reconciler.Reconcile(ctx, newTask("no-limits", nil))
		if err != nil {
			t.Fatalf("Reconcile without limits failed: %v", err)
		}
		if reconciled.Status.Phase != "Running" {
			t.Errorf("phase = %q, want Running", reconciled.Status.Phase)
		}
		if len(mockSrv.createdActors) != 1 {
			t.Errorf("fallback did not create the actor: %v", mockSrv.createdActors)
		}
	})
}

// assertCondition fails the test unless the task has a condition of the given type with
// the expected status and reason.
func assertCondition(t *testing.T, task *v1alpha1.Task, condType, wantStatus, wantReason string) {
	t.Helper()
	for _, c := range task.Status.Conditions {
		if c.Type != condType {
			continue
		}
		if c.Status != wantStatus || c.Reason != wantReason {
			t.Errorf("expected %s=%s (%s), got Status=%s Reason=%s", condType, wantStatus, wantReason, c.Status, c.Reason)
		}
		return
	}
	t.Errorf("expected %s condition to be set", condType)
}

func TestReconcileDelete_RemovesActorAndTemplates(t *testing.T) {
	ctx := context.Background()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	mockSrv := &mockControlServer{actorTemplates: map[string]bool{
		"job-tmpl-0a1b2c3d":               true, // current revision of task "job"
		"job-tmpl-deadbeef":               true, // stale revision of task "job"
		"job-tmpl-deadbeef-tmpl-01234567": true, // belongs to a task literally named "job-tmpl-deadbeef"
		"jobs-tmpl-0a1b2c3d":              true, // belongs to task "jobs"
		"default-template":                true,
	}}
	grpcServer := grpc.NewServer()
	ateapipb.RegisterControlServer(grpcServer, mockSrv)
	go grpcServer.Serve(lis)
	defer grpcServer.Stop()

	client, err := substrate.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	reconciler := controller.NewTaskReconciler(client, "test-template", "ax-system")
	reconciler.SecretResolver = noSecrets
	reconciler.WorkspaceReadyTimeout = 200 * time.Millisecond

	if err := reconciler.ReconcileDelete(ctx, "default", "job"); err != nil {
		t.Fatalf("ReconcileDelete failed: %v", err)
	}

	if len(mockSrv.deletedActors) != 1 || mockSrv.deletedActors[0] != "job" {
		t.Errorf("expected actor 'job' to be deleted, got %v", mockSrv.deletedActors)
	}

	wantDeleted := map[string]bool{"job-tmpl-0a1b2c3d": true, "job-tmpl-deadbeef": true}
	if len(mockSrv.deletedTemplates) != len(wantDeleted) {
		t.Errorf("expected %d templates deleted, got %v", len(wantDeleted), mockSrv.deletedTemplates)
	}
	for _, name := range mockSrv.deletedTemplates {
		if !wantDeleted[name] {
			t.Errorf("unexpected template deleted: %s", name)
		}
	}
	for _, keep := range []string{"job-tmpl-deadbeef-tmpl-01234567", "jobs-tmpl-0a1b2c3d", "default-template"} {
		if !mockSrv.actorTemplates[keep] {
			t.Errorf("template %s should not have been deleted", keep)
		}
	}
}
