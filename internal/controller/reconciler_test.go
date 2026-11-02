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
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/ax/internal/controller"
	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"go.ate.dev/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type mockControlServer struct {
	ateapipb.UnimplementedControlServer
	workerIP          string
	createdAtespaces  []string
	createdActors     []string
	resumedActors     []string
	suspendedActors   []string
	suspendErr        error
	deletedActors     []string
	actorTemplates    map[string]bool
	createdTemplates  []*ateapipb.ActorTemplate
	templateImages    []string
	deletedTemplates  []string
	getActorFunc      func(ctx context.Context, req *ateapipb.GetActorRequest) (*ateapipb.Actor, error)
	createAtespaceErr error
	createActorErr    error
	resumeActorErr    error
	crashedActor      string
	revertedActors    []string
	// createTemplateErr, when set, is returned by CreateActorTemplate to stand in
	// for Substrate rejecting a template (for example an invalid quantity).
	createTemplateErr error
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
	for _, c := range tmpl.GetContainers() {
		m.templateImages = append(m.templateImages, c.GetImage())
	}
	return tmpl, nil
}

func (m *mockControlServer) CreateAtespace(ctx context.Context, req *ateapipb.CreateAtespaceRequest) (*ateapipb.Atespace, error) {
	if m.createAtespaceErr != nil {
		return nil, m.createAtespaceErr
	}
	name := ""
	if req.Atespace != nil && req.Atespace.Metadata != nil {
		name = req.Atespace.Metadata.Name
	}
	m.createdAtespaces = append(m.createdAtespaces, name)
	return &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: name}}, nil
}

func (m *mockControlServer) CreateActor(ctx context.Context, req *ateapipb.CreateActorRequest) (*ateapipb.Actor, error) {
	if m.createActorErr != nil {
		return nil, m.createActorErr
	}
	name := ""
	if req.Actor != nil && req.Actor.Metadata != nil {
		name = req.Actor.Metadata.Name
	}
	if name == m.crashedActor {
		return nil, status.Error(codes.AlreadyExists, "actor exists")
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
	if m.resumeActorErr != nil {
		return nil, m.resumeActorErr
	}
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
					WorkerPod:    "worker-pod-1",
					WorkerPodIps: []string{wIP},
				},
			},
		},
		Resumed: true,
	}, nil
}

func (m *mockControlServer) SuspendActor(ctx context.Context, req *ateapipb.SuspendActorRequest) (*ateapipb.SuspendActorResponse, error) {
	if m.suspendErr != nil {
		return nil, m.suspendErr
	}
	name := ""
	if req.Actor != nil {
		name = req.Actor.Name
	}
	m.suspendedActors = append(m.suspendedActors, name)
	return &ateapipb.SuspendActorResponse{}, nil
}

func (m *mockControlServer) RevertActor(_ context.Context, req *ateapipb.RevertActorRequest) (*ateapipb.RevertActorResponse, error) {
	name := req.GetActor().GetName()
	m.revertedActors = append(m.revertedActors, name)
	return &ateapipb.RevertActorResponse{Actor: &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Name: name},
		Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED},
	}}, nil
}

func (m *mockControlServer) DeleteActor(ctx context.Context, req *ateapipb.DeleteActorRequest) (*ateapipb.Actor, error) {
	name := req.GetActor().GetName()
	m.deletedActors = append(m.deletedActors, name)
	return &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Name: name}}, nil
}

func (m *mockControlServer) GetActor(ctx context.Context, req *ateapipb.GetActorRequest) (*ateapipb.Actor, error) {
	if m.getActorFunc != nil {
		return m.getActorFunc(ctx, req)
	}
	name := req.GetActor().GetName()
	if name == m.crashedActor {
		return &ateapipb.Actor{
			Metadata: &ateapipb.ResourceMetadata{Name: name},
			Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_CRASHED},
		}, nil
	}
	for _, del := range m.deletedActors {
		if del == name {
			return nil, status.Errorf(codes.NotFound, "actor %q not found", name)
		}
	}
	for _, a := range m.createdActors {
		if a == name {
			return &ateapipb.Actor{
				Metadata: &ateapipb.ResourceMetadata{Name: name},
				Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
			}, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "actor %q not found", name)
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

	// 3. Resume the task
	reconciler := controller.NewTaskReconciler(client, "test-template", v1alpha1.DefaultSystemNamespace, "")
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
		// A client-supplied actor name is ignored: the actor is always named
		// after the task.
		Status: &v1alpha1.TaskStatus{Actor: "not-the-task"},
	}

	if err := reconciler.Resume(ctx, task, nil); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}

	// 4. Validate the results. The phase and worker IP belong to the actor, so
	// Resume leaves them for the server to read from Substrate.
	if st := task.GetStatus(); st.GetPhase() != "" || st.GetWorkerIp() != "" {
		t.Errorf("Resume set phase %q and worker IP %q, want both left empty", st.GetPhase(), st.GetWorkerIp())
	}
	// The mock's worker does not answer the readiness check.
	assertCondition(t, task, "WorkspaceReady", "False", v1alpha1.ReasonInitializing)

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

// TestTaskReconciler_DefaultTaskImage verifies that a task without spec.image is
// defaulted to the image passed to NewTaskReconciler (or the API default when
// none is given), and that the image
// is what ends up in the per-task ActorTemplate.
func TestTaskReconciler_DefaultTaskImage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override string
		want     string
	}{
		{name: "api default", want: v1alpha1.DefaultTaskImage},
		{name: "server override", override: "localhost:5001/ax-task-runner:e2e", want: "localhost:5001/ax-task-runner:e2e"},
	} {
		t.Run(tc.name, func(t *testing.T) {
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

			reconciler := controller.NewTaskReconciler(client, "test-template", v1alpha1.DefaultSystemNamespace, tc.override)
			reconciler.WorkspaceReadyTimeout = 200 * time.Millisecond

			task := &v1alpha1.Task{
				ApiVersion: v1alpha1.APIVersion,
				Kind:       v1alpha1.KindTask,
				Metadata:   &v1alpha1.ObjectMeta{Name: "no-image", Atespace: "default"},
				Spec:       &v1alpha1.TaskSpec{},
			}
			if err := reconciler.Resume(context.Background(), task, nil); err != nil {
				t.Fatalf("Resume failed: %v", err)
			}
			if got := task.GetSpec().GetImage(); got != tc.want {
				t.Errorf("spec.image = %q, want %q", got, tc.want)
			}
			if len(mockSrv.templateImages) != 1 || mockSrv.templateImages[0] != tc.want {
				t.Errorf("ActorTemplate images = %v, want [%q]", mockSrv.templateImages, tc.want)
			}
		})
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

	reconciler := controller.NewTaskReconciler(client, "test-template", v1alpha1.DefaultSystemNamespace, "")
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

	if err := reconciler.Suspend(ctx, task, nil); err != nil {
		t.Fatalf("Suspend failed: %v", err)
	}

	if st := task.GetStatus(); st.GetPhase() != "" || st.GetWorkerIp() != "" {
		t.Errorf("Suspend set phase %q and worker IP %q, want both left empty", st.GetPhase(), st.GetWorkerIp())
	}
	if len(mockSrv.suspendedActors) != 1 || mockSrv.suspendedActors[0] != "suspend-task" {
		t.Errorf("expected actor 'suspend-task' suspended, got %v", mockSrv.suspendedActors)
	}
	if len(mockSrv.resumedActors) != 0 {
		t.Errorf("expected no actor to be resumed, got %v", mockSrv.resumedActors)
	}
}

func TestTaskReconciler_SuspendError(t *testing.T) {
	ctx := context.Background()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	mockSrv := &mockControlServer{
		suspendErr: status.Error(codes.Unavailable, "substrate unavailable"),
	}
	grpcServer := grpc.NewServer()
	ateapipb.RegisterControlServer(grpcServer, mockSrv)
	go grpcServer.Serve(lis)
	defer grpcServer.Stop()

	client, err := substrate.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	reconciler := controller.NewTaskReconciler(client, "test-template", "ax-system", "")

	task := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata: &v1alpha1.ObjectMeta{
			Name:     "suspend-fail-task",
			Atespace: "default",
		},
		Spec: &v1alpha1.TaskSpec{
			Image: "ghrc.io/my-org/my-image",
		},
		Status: &v1alpha1.TaskStatus{
			// Set, so that ensureSubstrateResources has no task ID to fill in.
			Id:         "task-suspend-fail-task-1",
			WorkerIp:   "10.1.1.1",
			Conditions: []*v1alpha1.Condition{{Type: "Ready", Status: "True", Reason: v1alpha1.ReasonTaskRunning}},
		},
	}
	before := proto.Clone(task.Status).(*v1alpha1.TaskStatus)

	if err := reconciler.Suspend(ctx, task, nil); err == nil {
		t.Fatal("expected Suspend to return an error when SuspendActor fails")
	}
	if !proto.Equal(task.Status, before) {
		t.Errorf("Suspend changed the status after SuspendActor failed:\n got: %v\nwant: %v", task.Status, before)
	}
}

// TestTaskReconciler_TemplateCreationFailed covers google/ax#366: when Substrate
// rejects the per-task ActorTemplate (for example because the image is referenced
// by tag rather than digest), the task must report that error directly instead of
// silently falling back to the default template.
func TestTaskReconciler_TemplateCreationFailed(t *testing.T) {
	ctx := t.Context()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	const substrateMsg = `actor_template.containers[0].image: Invalid value: "localhost:5001/my-agent:v1": must be pinned by digest (changing the image invalidates snapshots)`
	mockSrv := &mockControlServer{
		createTemplateErr: status.Error(codes.InvalidArgument, substrateMsg),
	}
	grpcServer := grpc.NewServer()
	ateapipb.RegisterControlServer(grpcServer, mockSrv)
	go grpcServer.Serve(lis)
	defer grpcServer.Stop()

	client, err := substrate.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	reconciler := controller.NewTaskReconciler(client, "test-template", "ax-system", "")
	reconciler.WorkspaceReadyTimeout = 200 * time.Millisecond

	task := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata: &v1alpha1.ObjectMeta{
			Name:     "tagged-image",
			Atespace: "default",
		},
		Spec: &v1alpha1.TaskSpec{
			Image:   "localhost:5001/my-agent:v1",
			Command: []string{"python3", "/app/agent.py"},
		},
	}

	err = reconciler.Resume(ctx, task, nil)
	if err == nil {
		t.Fatal("expected Resume to fail when the ActorTemplate cannot be created")
	}
	if !strings.Contains(err.Error(), "must be pinned by digest") {
		t.Errorf("expected error to carry the Substrate message, got %v", err)
	}

	assertCondition(t, task, "Ready", "False", v1alpha1.ReasonTemplateCreationFailed)
	for _, c := range task.Status.Conditions {
		if c.Type != "Ready" {
			continue
		}
		// The user-facing message should be exactly what Substrate said, without the
		// "rpc error: code = ... desc = " wrapper or our internal context prefix.
		if c.Message != substrateMsg {
			t.Errorf("expected Ready message to be the bare Substrate error\n got: %q\nwant: %q", c.Message, substrateMsg)
		}
	}

	// Resume must stop before touching the actor: no fallback to the default template.
	if len(mockSrv.createdActors) != 0 {
		t.Errorf("expected no actor to be created, got %v", mockSrv.createdActors)
	}
	if len(mockSrv.resumedActors) != 0 {
		t.Errorf("expected no actor to be resumed, got %v", mockSrv.resumedActors)
	}
}

// TestTaskReconciler_TemplateCreationTransient checks that a transient Substrate
// error during ActorTemplate creation (for example Unavailable after retries are
// exhausted) is recorded as SubstrateUnavailable rather than TemplateCreationFailed.
// The server keeps a task that failed this way Pending for a retry.
func TestTaskReconciler_TemplateCreationTransient(t *testing.T) {
	ctx := t.Context()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	// DeadlineExceeded is transient per substrate.IsTransientError but is not in the
	// client's retry list, so the test does not wait out the Unavailable backoff.
	mockSrv := &mockControlServer{
		createTemplateErr: status.Error(codes.DeadlineExceeded, "substrate control plane timed out"),
	}
	grpcServer := grpc.NewServer()
	ateapipb.RegisterControlServer(grpcServer, mockSrv)
	go grpcServer.Serve(lis)
	defer grpcServer.Stop()

	client, err := substrate.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	reconciler := controller.NewTaskReconciler(client, "test-template", "ax-system", "")
	reconciler.WorkspaceReadyTimeout = 200 * time.Millisecond

	task := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata: &v1alpha1.ObjectMeta{
			Name:     "template-transient",
			Atespace: "default",
		},
		Spec: &v1alpha1.TaskSpec{Image: "ghcr.io/my-org/my-agent@sha256:0000000000000000000000000000000000000000000000000000000000000000"},
	}

	if err := reconciler.Resume(ctx, task, nil); err == nil {
		t.Fatal("expected Resume to return an error on transient template failure")
	}
	assertCondition(t, task, "Ready", "False", v1alpha1.ReasonSubstrateUnavailable)
	if len(mockSrv.createdActors) != 0 {
		t.Errorf("expected no actor to be created, got %v", mockSrv.createdActors)
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

	reconciler := controller.NewTaskReconciler(client, "test-template", v1alpha1.DefaultSystemNamespace, "")
	reconciler.WorkspaceReadyTimeout = 200 * time.Millisecond

	task := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata: &v1alpha1.ObjectMeta{
			Name:     "ready-task",
			Atespace: "default",
		},
		Spec: &v1alpha1.TaskSpec{},
	}

	// Case 1: Worker not responding on readyz -> WorkspaceReady=False.
	if err := reconciler.Resume(ctx, task, nil); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	assertCondition(t, task, "WorkspaceReady", "False", v1alpha1.ReasonInitializing)

	// Case 2: Worker readyz endpoint succeeds -> WorkspaceReady=True.
	mockSrv.workerIP = httpLis.Addr().String()
	if err := reconciler.Resume(ctx, task, nil); err != nil {
		t.Fatalf("Resume with ready worker failed: %v", err)
	}
	assertCondition(t, task, "WorkspaceReady", "True", v1alpha1.ReasonSetupComplete)

	// Case 3: Suspending the task -> the workspace was already initialized, so
	// WorkspaceReady stays True.
	if err := reconciler.Suspend(ctx, task, nil); err != nil {
		t.Fatalf("Suspend failed: %v", err)
	}
	assertCondition(t, task, "WorkspaceReady", "True", v1alpha1.ReasonSetupComplete)

	// Case 4: Resuming with the worker unreachable -> the reconciler trusts the recorded
	// WorkspaceReady instead of re-polling.
	mockSrv.workerIP = "127.0.0.1:1"
	if err := reconciler.Resume(ctx, task, nil); err != nil {
		t.Fatalf("Resume after suspend failed: %v", err)
	}
	assertCondition(t, task, "WorkspaceReady", "True", v1alpha1.ReasonSetupComplete)
	if got := len(mockSrv.actorTemplates); got != 1 {
		t.Fatalf("readiness updates and suspend/resume created %d templates, want 1", got)
	}

	// A launch configuration change must still get a distinct template.
	task.Spec.Command = []string{"python3", "agent.py"}
	if err := reconciler.Resume(ctx, task, nil); err != nil {
		t.Fatalf("Resume with changed command failed: %v", err)
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

	reconciler := controller.NewTaskReconciler(client, "test-template", "ax-system", "")
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
			Resources: &v1alpha1.ResourcesSpec{
				Limits: &v1alpha1.ResourceList{Cpu: "2", Memory: "4Gi"},
			},
		},
	}

	if err := reconciler.Suspend(ctx, task, nil); err != nil {
		t.Fatalf("Suspend failed: %v", err)
	}
	if len(mockSrv.createdTemplates) != 1 {
		t.Fatalf("created %d templates, want 1", len(mockSrv.createdTemplates))
	}
	want := &ateapipb.ResourceRequirements{Limits: &ateapipb.Resources{
		CpuMillicores: 2000,
		MemoryBytes:   4 << 30,
	}}
	if got := mockSrv.createdTemplates[0].GetResources(); !proto.Equal(got, want) {
		t.Errorf("template resources = %v, want %v", got, want)
	}

	// Raising a limit is a launch configuration change and must provision a new
	// template, under a new name, carrying the new value.
	task.Spec.Resources.Limits.Memory = "8Gi"
	if err := reconciler.Suspend(ctx, task, nil); err != nil {
		t.Fatalf("Suspend with changed limits failed: %v", err)
	}
	if len(mockSrv.createdTemplates) != 2 {
		t.Fatalf("limits change left %d templates, want 2", len(mockSrv.createdTemplates))
	}
	first, second := mockSrv.createdTemplates[0].GetMetadata().GetName(), mockSrv.createdTemplates[1].GetMetadata().GetName()
	if first == second {
		t.Errorf("limits change reused template name %q, want a distinct name", first)
	}
	want.Limits.MemoryBytes = 8 << 30
	if got := mockSrv.createdTemplates[1].GetResources(); !proto.Equal(got, want) {
		t.Errorf("template resources after change = %v, want %v", got, want)
	}

	// A task without limits uses the default CPU and memory limits.
	plain := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata:   &v1alpha1.ObjectMeta{Name: "plain-task", Atespace: "default"},
		Spec:       &v1alpha1.TaskSpec{Image: "ghrc.io/my-org/my-image"},
	}
	if err := reconciler.Suspend(ctx, plain, nil); err != nil {
		t.Fatalf("Suspend of task without limits failed: %v", err)
	}
	wantDefault := &ateapipb.ResourceRequirements{Limits: &ateapipb.Resources{
		CpuMillicores: substrate.DefaultCPUMillicores,
		MemoryBytes:   substrate.DefaultMemoryBytes,
	}}
	if got := mockSrv.createdTemplates[len(mockSrv.createdTemplates)-1].GetResources(); !proto.Equal(got, wantDefault) {
		t.Errorf("template for task without limits has resources %v, want %v", got, wantDefault)
	}
}

// A task that asks for limits must never run without them: invalid limits and a
// Substrate rejection of the template both fail the reconcile instead of falling
// back to the default template. Without limits the fallback contract is unchanged.
func TestTaskReconciler_ResourceLimitsFailurePaths(t *testing.T) {
	ctx := context.Background()

	newTask := func(name string, resources *v1alpha1.ResourcesSpec) *v1alpha1.Task {
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
		reconciler := controller.NewTaskReconciler(client, "test-template", "ax-system", "")
		reconciler.WorkspaceReadyTimeout = 200 * time.Millisecond
		return reconciler
	}

	t.Run("invalid limits fail before provisioning", func(t *testing.T) {
		mockSrv := &mockControlServer{}
		reconciler := setup(t, mockSrv)

		for _, reqs := range []*v1alpha1.ResourcesSpec{
			{Limits: &v1alpha1.ResourceList{Cpu: "two"}},
			{Limits: &v1alpha1.ResourceList{Cpu: "0"}},
			{Limits: &v1alpha1.ResourceList{Cpu: "1000"}},
			{Limits: &v1alpha1.ResourceList{Memory: "-4Gi"}},
			{Requests: &v1alpha1.ResourceList{Cpu: "500m"}, Limits: &v1alpha1.ResourceList{Cpu: "2"}},
		} {
			task := newTask("bad-limits", reqs)
			if err := reconciler.Suspend(ctx, task, nil); err == nil {
				t.Errorf("Suspend(%v) succeeded, want error", reqs)
			}
			assertCondition(t, task, "Ready", "False", v1alpha1.ReasonInvalidResources)
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

		task := newTask("rejected-limits", &v1alpha1.ResourcesSpec{
			Limits: &v1alpha1.ResourceList{Cpu: "2", Memory: "4Gi"},
		})
		if err := reconciler.Suspend(ctx, task, nil); err == nil {
			t.Fatal("Suspend succeeded although the template with limits was rejected")
		}
		assertCondition(t, task, "Ready", "False", v1alpha1.ReasonTemplateCreationFailed)
		if len(mockSrv.createdActors) != 0 || len(mockSrv.resumedActors) != 0 {
			t.Errorf("task ran on a fallback template without its limits: actors=%v resumed=%v", mockSrv.createdActors, mockSrv.resumedActors)
		}
	})

	t.Run("rejected template without limits does not fall back either", func(t *testing.T) {
		mockSrv := &mockControlServer{
			createTemplateErr: status.Error(codes.InvalidArgument, "image must be pinned by digest"),
		}
		reconciler := setup(t, mockSrv)

		task := newTask("no-limits", nil)
		if err := reconciler.Suspend(ctx, task, nil); err == nil {
			t.Fatal("Suspend succeeded although the template was rejected")
		}
		// Falling back would run a different image than the task asked for, so the
		// template error is terminal regardless of whether limits are set (#366).
		assertCondition(t, task, "Ready", "False", v1alpha1.ReasonTemplateCreationFailed)
		if len(mockSrv.createdActors) != 0 {
			t.Errorf("task ran on a fallback template: actors=%v", mockSrv.createdActors)
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

func TestDelete_RemovesActorAndTemplates(t *testing.T) {
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

	reconciler := controller.NewTaskReconciler(client, "test-template", v1alpha1.DefaultSystemNamespace, "")
	reconciler.WorkspaceReadyTimeout = 200 * time.Millisecond

	if err := reconciler.Delete(ctx, "default", "job"); err != nil {
		t.Fatalf("Delete failed: %v", err)
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

func TestDelete_BlocksUntilActorDeleted(t *testing.T) {
	ctx := context.Background()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	getCalls := 0
	mockSrv := &mockControlServer{
		getActorFunc: func(ctx context.Context, req *ateapipb.GetActorRequest) (*ateapipb.Actor, error) {
			getCalls++
			if getCalls < 3 {
				// Simulate actor in deleting state during the first two poll checks
				return &ateapipb.Actor{
					Metadata: &ateapipb.ResourceMetadata{Name: req.GetActor().GetName()},
					Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_DELETING},
				}, nil
			}
			// Once actor is completely torn down
			return nil, status.Errorf(codes.NotFound, "actor %q not found", req.GetActor().GetName())
		},
	}
	grpcServer := grpc.NewServer()
	ateapipb.RegisterControlServer(grpcServer, mockSrv)
	go grpcServer.Serve(lis)
	defer grpcServer.Stop()

	client, err := substrate.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	reconciler := controller.NewTaskReconciler(client, "test-template", v1alpha1.DefaultSystemNamespace, "")

	if err := reconciler.Delete(ctx, "default", "slow-delete-task"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	if getCalls < 3 {
		t.Errorf("expected at least 3 GetActor calls before deletion completes, got %d", getCalls)
	}
}

func TestDelete_ActorDeletionTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	mockSrv := &mockControlServer{
		getActorFunc: func(ctx context.Context, req *ateapipb.GetActorRequest) (*ateapipb.Actor, error) {
			// Actor remains in deleting state indefinitely
			return &ateapipb.Actor{
				Metadata: &ateapipb.ResourceMetadata{Name: req.GetActor().GetName()},
				Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_DELETING},
			}, nil
		},
	}
	grpcServer := grpc.NewServer()
	ateapipb.RegisterControlServer(grpcServer, mockSrv)
	go grpcServer.Serve(lis)
	defer grpcServer.Stop()

	client, err := substrate.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	reconciler := controller.NewTaskReconciler(client, "test-template", v1alpha1.DefaultSystemNamespace, "")

	err = reconciler.Delete(ctx, "default", "stuck-task")
	if err == nil {
		t.Fatal("expected error due to timeout, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("expected deadline exceeded error, got %v", err)
	}
}

func TestEnsureActor_RevertsCrashedActor(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	mockSrv := &mockControlServer{crashedActor: "job"}
	grpcServer := grpc.NewServer()
	ateapipb.RegisterControlServer(grpcServer, mockSrv)
	go grpcServer.Serve(lis)
	defer grpcServer.Stop()

	client, err := substrate.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	actor, err := client.EnsureActor(context.Background(), "default", "job", "default", "job-tmpl-0a1b2c3d")
	if err != nil {
		t.Fatalf("EnsureActor failed: %v", err)
	}
	if got := actor.GetStatus().GetState(); got != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Errorf("actor state = %v, want SUSPENDED", got)
	}
	if len(mockSrv.revertedActors) != 1 || len(mockSrv.deletedActors) != 0 {
		t.Errorf("crashed actor: reverted %v, deleted %v; want reverted only", mockSrv.revertedActors, mockSrv.deletedActors)
	}
}

func TestTaskReconciler_TransientSubstrateUnavailable(t *testing.T) {
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

	reconciler := controller.NewTaskReconciler(client, "test-template", v1alpha1.DefaultSystemNamespace, "")
	reconciler.WorkspaceReadyTimeout = 50 * time.Millisecond

	// 1. A transient error during EnsureAtespace is recorded with Reason="SubstrateUnavailable".
	mockSrv.createAtespaceErr = status.Error(codes.DeadlineExceeded, "substrate control plane timed out")
	task := &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "transient-atespace"},
		Spec:     &v1alpha1.TaskSpec{Image: "ghrc.io/my-org/my-image"},
	}
	if err := reconciler.Suspend(ctx, task, nil); err == nil {
		t.Fatal("expected error from Suspend when EnsureAtespace fails, got nil")
	}
	assertCondition(t, task, "Ready", "False", v1alpha1.ReasonSubstrateUnavailable)
	mockSrv.createAtespaceErr = nil

	// 2. A transient error during EnsureActor is recorded with Reason="SubstrateUnavailable".
	mockSrv.createActorErr = status.Error(codes.DeadlineExceeded, "substrate actor creation timed out")
	task2 := &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "transient-actor"},
		Spec:     &v1alpha1.TaskSpec{Image: "ghrc.io/my-org/my-image"},
	}
	if err := reconciler.Suspend(ctx, task2, nil); err == nil {
		t.Fatal("expected error from Suspend when EnsureActor fails, got nil")
	}
	assertCondition(t, task2, "Ready", "False", v1alpha1.ReasonSubstrateUnavailable)
	mockSrv.createActorErr = nil

	// 3. A transient error during ResumeActor is recorded with Reason="SubstrateUnavailable",
	// and a subsequent Resume once Substrate recovers resumes the actor.
	mockSrv.resumeActorErr = status.Error(codes.DeadlineExceeded, "substrate worker pool temporarily unreachable")
	task3 := &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "transient-resume"},
		Spec:     &v1alpha1.TaskSpec{Image: "ghrc.io/my-org/my-image"},
	}
	if err := reconciler.Resume(ctx, task3, nil); err == nil {
		t.Fatal("expected error from Resume when ResumeActor fails, got nil")
	}
	assertCondition(t, task3, "Ready", "False", v1alpha1.ReasonSubstrateUnavailable)

	// Recovery on retry:
	mockSrv.resumeActorErr = nil
	if err := reconciler.Resume(ctx, task3, nil); err != nil {
		t.Fatalf("expected Resume to succeed after Substrate recovers, got %v", err)
	}
	if len(mockSrv.resumedActors) != 1 || mockSrv.resumedActors[0] != "transient-resume" {
		t.Errorf("expected actor 'transient-resume' resumed after recovery, got %v", mockSrv.resumedActors)
	}

	// 4. A permanent error is recorded with its own reason.
	mockSrv.createActorErr = status.Error(codes.InvalidArgument, "invalid actor specification")
	task4 := &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "permanent-actor-err"},
		Spec:     &v1alpha1.TaskSpec{Image: "ghrc.io/my-org/my-image"},
	}
	if err := reconciler.Suspend(ctx, task4, nil); err == nil {
		t.Fatal("expected error on InvalidArgument, got nil")
	}
	assertCondition(t, task4, "Ready", "False", v1alpha1.ReasonActorCreationFailed)
	mockSrv.createActorErr = nil
}
