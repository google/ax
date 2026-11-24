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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/ax/internal/controller"
	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/internal/workspace"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/google/ax/runner"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// readyRunnerAddr is the address of a runner, shared by the tests, that
// accepts every push and passes every readiness check.
var readyRunnerAddr = sync.OnceValue(func() string {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+v1alpha1.RunnerTaskPath, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return strings.TrimPrefix(httptest.NewServer(mux).URL, "http://")
})

type mockControlServer struct {
	ateapipb.UnimplementedControlServer
	createdAtespaces []string
	createdActors    []string
	resumedActors    []string
	suspendedActors  []string
	suspendErr       error
	deletedActors    []string
	actorTemplates   map[string]bool
	// templateRefs records every ActorTemplate ref looked up via GetActorTemplate.
	templateRefs []*ateapipb.ObjectRef
	// actorTemplateRefs records the ActorTemplate ref of every created actor.
	actorTemplateRefs []*ateapipb.ObjectRef
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
	createTemplateErr     error
	createdEgressPolicies []*ateapipb.CreateActorEgressPolicyRequest
	// egressPolicyCalls counts the egress policy calls of any kind.
	egressPolicyCalls int
}

func (m *mockControlServer) GetActorEgressPolicy(_ context.Context, req *ateapipb.GetActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
	m.egressPolicyCalls++
	for _, created := range m.createdEgressPolicies {
		if proto.Equal(created.GetActor(), req.GetActor()) {
			return created.GetEgressPolicy(), nil
		}
	}
	return nil, status.Error(codes.NotFound, "egress policy not found")
}

func (m *mockControlServer) CreateActorEgressPolicy(_ context.Context, req *ateapipb.CreateActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
	m.egressPolicyCalls++
	m.createdEgressPolicies = append(m.createdEgressPolicies, req)
	return req.GetEgressPolicy(), nil
}

func (m *mockControlServer) GetActorTemplate(_ context.Context, req *ateapipb.GetActorTemplateRequest) (*ateapipb.ActorTemplate, error) {
	ref := req.GetActorTemplate()
	m.templateRefs = append(m.templateRefs, ref)
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
	m.actorTemplateRefs = append(m.actorTemplateRefs, req.GetActor().GetActorTemplate())
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
	return &ateapipb.ResumeActorResponse{
		Actor: &ateapipb.Actor{
			Metadata: &ateapipb.ResourceMetadata{Name: name},
			Status: &ateapipb.ActorStatus{
				State: ateapipb.ActorState_ACTOR_STATE_RUNNING,
				WorkerAssignment: &ateapipb.WorkerAssignment{
					WorkerPod:    "worker-pod-1",
					WorkerPodIps: []string{"10.244.1.42"},
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

func TestTaskManager(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ATENET_ROUTER_ADDR", readyRunnerAddr())

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
	client, err := substrate.NewClientWithOptions(substrate.ClientOptions{Target: lis.Addr().String(), Plaintext: true})
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	// 3. Resume the task
	manager := controller.NewTaskManager(client)
	manager.WorkspaceReadyTimeout = 200 * time.Millisecond

	task := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata: &v1alpha1.ObjectMeta{
			Name:     "test-task",
			Atespace: "default",
		},
		Spec: &v1alpha1.TaskSpec{
			Profile:  &v1alpha1.Profile{Image: "ghrc.io/my-org/my-image"},
			Commands: []string{"/bin/task-runner"},
		},
		// A client-supplied actor name is ignored: the actor is always named
		// after the task.
		Status: &v1alpha1.TaskStatus{Actor: "not-the-task"},
	}

	if err := manager.Resume(ctx, task, nil, nil); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}

	// 4. Validate the results. The state belongs to the actor, so Resume leaves
	// it unspecified for the server to read from Substrate.
	if st := task.GetStatus(); st.GetState() != v1alpha1.TaskState_TASK_STATE_UNSPECIFIED {
		t.Errorf("Resume set state %v, want UNSPECIFIED", st.GetState())
	}
	if got := task.GetStatus().GetResolvedArgv(); len(got) != 1 || got[0] != "/bin/task-runner" {
		t.Errorf("ResolvedArgv = %v, want [/bin/task-runner]", got)
	}
	if got := task.GetStatus().GetAteTemplate(); !strings.HasPrefix(got.GetName(), "ax-") || got.GetAtespace() != "default" {
		t.Errorf("AteTemplate = %v, want {Name: ax-*, Atespace: default}", got)
	}
	// The mock's runner accepts the task and reports it ready.
	assertCondition(t, task, "WorkspaceReady", "True", v1alpha1.ReasonSetupComplete)

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

// TestTaskManager_MissingImage verifies that a task whose profile names no
// image or ateTemplate fails with TemplateCreationFailed instead of creating a
// template.
func TestTaskManager_MissingImage(t *testing.T) {
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

	client, err := substrate.NewClientWithOptions(substrate.ClientOptions{Target: lis.Addr().String(), Plaintext: true})
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	manager := controller.NewTaskManager(client)
	manager.WorkspaceReadyTimeout = 200 * time.Millisecond

	task := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata:   &v1alpha1.ObjectMeta{Name: "no-image", Atespace: "default"},
		Spec:       &v1alpha1.TaskSpec{},
	}
	if err := manager.Resume(context.Background(), task, nil, nil); err == nil {
		t.Fatal("Resume succeeded, want an error for a task without an image")
	}
	assertCondition(t, task, "Ready", "False", v1alpha1.ReasonTemplateCreationFailed)
	if len(mockSrv.createdTemplates) != 0 {
		t.Errorf("created %d ActorTemplates, want 0", len(mockSrv.createdTemplates))
	}
}

// TestTaskManager_RunnerVolumeImage verifies that a configured runner volume
// image is mounted into the task's template and run from there, that the
// runner digest is part of the template name, and that leaving it unset keeps
// today's template (and its name) unchanged.
func TestTaskManager_RunnerVolumeImage(t *testing.T) {
	resume := func(t *testing.T, runner string) *ateapipb.ActorTemplate {
		t.Helper()
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("failed to listen: %v", err)
		}
		t.Cleanup(func() { lis.Close() })
		mockSrv := &mockControlServer{}
		grpcServer := grpc.NewServer()
		ateapipb.RegisterControlServer(grpcServer, mockSrv)
		go grpcServer.Serve(lis)
		t.Cleanup(grpcServer.Stop)
		client, err := substrate.NewClientWithOptions(substrate.ClientOptions{Target: lis.Addr().String(), Plaintext: true})
		if err != nil {
			t.Fatalf("failed to create substrate client: %v", err)
		}
		t.Cleanup(func() { client.Close() })

		manager := controller.NewTaskManager(client)
		manager.WorkspaceReadyTimeout = 200 * time.Millisecond
		manager.RunnerVolumeImage = runner
		task := &v1alpha1.Task{
			ApiVersion: v1alpha1.APIVersion,
			Kind:       v1alpha1.KindTask,
			Metadata:   &v1alpha1.ObjectMeta{Name: "pure", Atespace: "default"},
			Spec:       &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "ghcr.io/agent@sha256:a"}},
		}
		if err := manager.Resume(context.Background(), task, nil, nil); err != nil {
			t.Fatalf("Resume failed: %v", err)
		}
		if len(mockSrv.createdTemplates) != 1 {
			t.Fatalf("created %d templates, want 1", len(mockSrv.createdTemplates))
		}
		return mockSrv.createdTemplates[0]
	}

	const runner1 = "ghcr.io/google/ax/ax-runner-volume@sha256:r1"
	tmpl := resume(t, runner1)
	c := tmpl.GetContainers()[0]
	if got, want := c.GetCommand(), v1alpha1.RunnerMountPath+"/ax-runner"; len(got) != 1 || got[0] != want {
		t.Errorf("command = %v, want [%s]", got, want)
	}
	var mounted bool
	for _, v := range tmpl.GetVolumes() {
		mounted = mounted || v.GetImage().GetReference() == runner1
	}
	if !mounted {
		t.Errorf("volumes = %v, want an image volume of %q", tmpl.GetVolumes(), runner1)
	}

	if other := resume(t, "ghcr.io/google/ax/ax-runner-volume@sha256:r2"); other.GetMetadata().GetName() == tmpl.GetMetadata().GetName() {
		t.Errorf("a new runner digest kept template name %q", tmpl.GetMetadata().GetName())
	}

	baked := resume(t, "")
	if baked.GetMetadata().GetName() == tmpl.GetMetadata().GetName() {
		t.Errorf("unset runner volume image kept template name %q", tmpl.GetMetadata().GetName())
	}
	if got := baked.GetContainers()[0].GetCommand(); len(got) != 1 || got[0] != substrate.DefaultGuestCommand {
		t.Errorf("unset runner volume image: command = %v, want [%s]", got, substrate.DefaultGuestCommand)
	}
	for _, v := range baked.GetVolumes() {
		if v.GetImage() != nil {
			t.Errorf("unset runner volume image: unexpected image volume %v", v)
		}
	}
}

func TestTaskManager_Suspend(t *testing.T) {
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

	client, err := substrate.NewClientWithOptions(substrate.ClientOptions{Target: lis.Addr().String(), Plaintext: true})
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	manager := controller.NewTaskManager(client)
	manager.WorkspaceReadyTimeout = 200 * time.Millisecond

	task := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata: &v1alpha1.ObjectMeta{
			Name:     "suspend-task",
			Atespace: "default",
		},
		Spec: &v1alpha1.TaskSpec{
			Profile: &v1alpha1.Profile{Image: "ghrc.io/my-org/my-image"},
		},
	}

	if err := manager.Suspend(ctx, task, nil, nil); err != nil {
		t.Fatalf("Suspend failed: %v", err)
	}

	if st := task.GetStatus(); st.GetState() != v1alpha1.TaskState_TASK_STATE_UNSPECIFIED {
		t.Errorf("Suspend set state %v, want UNSPECIFIED", st.GetState())
	}
	if len(mockSrv.suspendedActors) != 1 || mockSrv.suspendedActors[0] != "suspend-task" {
		t.Errorf("expected actor 'suspend-task' suspended, got %v", mockSrv.suspendedActors)
	}
	if len(mockSrv.resumedActors) != 0 {
		t.Errorf("expected no actor to be resumed, got %v", mockSrv.resumedActors)
	}
}

// The actor of a task that binds a gateway gets that gateway's rules. A task
// that binds none still gets a policy, for the runner rule.
func TestTaskManager_GatewayRules(t *testing.T) {
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

	client, err := substrate.NewClientWithOptions(substrate.ClientOptions{Target: lis.Addr().String(), Plaintext: true})
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	manager := controller.NewTaskManager(client)
	manager.WorkspaceReadyTimeout = 200 * time.Millisecond

	newTask := func(name string) *v1alpha1.Task {
		return &v1alpha1.Task{
			ApiVersion: v1alpha1.APIVersion,
			Kind:       v1alpha1.KindTask,
			Metadata:   &v1alpha1.ObjectMeta{Name: name, Atespace: "default"},
			Spec:       &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "ghrc.io/my-org/my-image"}},
		}
	}

	unbound := newTask("unbound-task")
	if err := manager.Suspend(ctx, unbound, nil, nil); err != nil {
		t.Fatalf("Suspend failed: %v", err)
	}
	if err := manager.Resume(ctx, unbound, nil, nil); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	if n := mockSrv.egressPolicyCalls; n != 0 {
		t.Fatalf("made %d egress policy calls for a task that binds no gateway, want none", n)
	}

	gw := &v1alpha1.Gateway{
		Metadata: &v1alpha1.ObjectMeta{Name: "gw", Atespace: "default"},
		Spec: &v1alpha1.GatewaySpec{Egress: []*ateapipb.EgressRule{{
			Https: &ateapipb.HttpsRule{Hostnames: []string{"a.example.com"}},
		}}},
	}
	bound := newTask("bound-task")
	bound.Spec.Gateway = &v1alpha1.GatewayRef{Name: "gw"}
	if err := manager.Suspend(ctx, bound, nil, gw); err != nil {
		t.Fatalf("Suspend failed: %v", err)
	}
	if n := len(mockSrv.createdEgressPolicies); n != 1 {
		t.Fatalf("created %d egress policies for a task that binds a gateway, want 1", n)
	}
	req := mockSrv.createdEgressPolicies[0]
	if actor, policy := req.GetActor(), req.GetEgressPolicy(); actor.GetAtespace() != "default" || actor.GetName() != "bound-task" ||
		policy.GetMetadata().GetName() != "default" || len(policy.GetRules()) != 1 || policy.GetRules()[0].GetHttps().GetHostnames()[0] != "a.example.com" {
		t.Errorf("egress policy request = %v, want the gateway's rules as policy \"default\" of actor default/bound-task", req)
	}
}

func TestTaskManager_SuspendError(t *testing.T) {
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

	client, err := substrate.NewClientWithOptions(substrate.ClientOptions{Target: lis.Addr().String(), Plaintext: true})
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	manager := controller.NewTaskManager(client)

	task := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata: &v1alpha1.ObjectMeta{
			Name:     "suspend-fail-task",
			Atespace: "default",
		},
		Spec: &v1alpha1.TaskSpec{
			Profile: &v1alpha1.Profile{Image: "ghrc.io/my-org/my-image"},
		},
		Status: &v1alpha1.TaskStatus{
			Conditions: []*v1alpha1.Condition{{Type: "Ready", Status: "True", Reason: v1alpha1.ReasonTaskRunning}},
		},
	}
	before := proto.Clone(task.Status).(*v1alpha1.TaskStatus)

	if err := manager.Suspend(ctx, task, nil, nil); err == nil {
		t.Fatal("expected Suspend to return an error when SuspendActor fails")
	}
	if !proto.Equal(task.Status, before) {
		t.Errorf("Suspend changed the status after SuspendActor failed:\n got: %v\nwant: %v", task.Status, before)
	}
}

// TestTaskManager_TemplateCreationFailed covers google/ax#366: when Substrate
// rejects the per-task ActorTemplate (for example because the image is referenced
// by tag rather than digest), the task must report that error directly instead of
// silently falling back to the default template.
func TestTaskManager_TemplateCreationFailed(t *testing.T) {
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

	client, err := substrate.NewClientWithOptions(substrate.ClientOptions{Target: lis.Addr().String(), Plaintext: true})
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	manager := controller.NewTaskManager(client)
	manager.WorkspaceReadyTimeout = 200 * time.Millisecond

	task := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata: &v1alpha1.ObjectMeta{
			Name:     "tagged-image",
			Atespace: "default",
		},
		Spec: &v1alpha1.TaskSpec{
			Profile:  &v1alpha1.Profile{Image: "localhost:5001/my-agent:v1"},
			Commands: []string{"python3", "/app/agent.py"},
		},
	}

	err = manager.Resume(ctx, task, nil, nil)
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

// TestTaskManager_TemplateCreationTransient checks that a transient Substrate
// error during ActorTemplate creation (for example Unavailable after retries are
// exhausted) is recorded as SubstrateUnavailable rather than TemplateCreationFailed.
// The server keeps a task that failed this way Pending for a retry.
func TestTaskManager_TemplateCreationTransient(t *testing.T) {
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

	client, err := substrate.NewClientWithOptions(substrate.ClientOptions{Target: lis.Addr().String(), Plaintext: true})
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	manager := controller.NewTaskManager(client)
	manager.WorkspaceReadyTimeout = 200 * time.Millisecond

	task := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata: &v1alpha1.ObjectMeta{
			Name:     "template-transient",
			Atespace: "default",
		},
		Spec: &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "ghcr.io/my-org/my-agent@sha256:0000000000000000000000000000000000000000000000000000000000000000"}},
	}

	if err := manager.Resume(ctx, task, nil, nil); err == nil {
		t.Fatal("expected Resume to return an error on transient template failure")
	}
	assertCondition(t, task, "Ready", "False", v1alpha1.ReasonSubstrateUnavailable)
	if len(mockSrv.createdActors) != 0 {
		t.Errorf("expected no actor to be created, got %v", mockSrv.createdActors)
	}
}

func TestTaskManager_WorkspaceReady(t *testing.T) {
	ctx := context.Background()

	// 1. Mock Substrate Control Server
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	// 2. A runner that accepts the task but is still setting up its workspace
	// until workspaceDone is set.
	var workspaceDone atomic.Bool
	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !workspaceDone.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	httpMux.HandleFunc("POST "+v1alpha1.RunnerTaskPath, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	runnerSrv := httptest.NewServer(httpMux)
	defer runnerSrv.Close()
	t.Setenv("ATENET_ROUTER_ADDR", runnerSrv.URL)

	mockSrv := &mockControlServer{}
	grpcServer := grpc.NewServer()
	ateapipb.RegisterControlServer(grpcServer, mockSrv)
	go grpcServer.Serve(lis)
	defer grpcServer.Stop()

	client, err := substrate.NewClientWithOptions(substrate.ClientOptions{Target: lis.Addr().String(), Plaintext: true})
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	manager := controller.NewTaskManager(client)
	manager.WorkspaceReadyTimeout = 200 * time.Millisecond

	task := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata: &v1alpha1.ObjectMeta{
			Name:     "ready-task",
			Atespace: "default",
		},
		Spec: &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "ghcr.io/agent@sha256:a"}},
	}

	// Case 1: The runner took the task but its workspace is not set up within
	// the timeout -> the resume succeeds with WorkspaceReady=False.
	if err := manager.Resume(ctx, task, nil, nil); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	assertCondition(t, task, "WorkspaceReady", "False", v1alpha1.ReasonInitializing)

	// Case 2: Worker readyz endpoint succeeds -> WorkspaceReady=True.
	workspaceDone.Store(true)
	if err := manager.Resume(ctx, task, nil, nil); err != nil {
		t.Fatalf("Resume with ready worker failed: %v", err)
	}
	assertCondition(t, task, "WorkspaceReady", "True", v1alpha1.ReasonSetupComplete)

	// Case 3: Suspending the task -> the workspace was already initialized, so
	// WorkspaceReady stays True.
	if err := manager.Suspend(ctx, task, nil, nil); err != nil {
		t.Fatalf("Suspend failed: %v", err)
	}
	assertCondition(t, task, "WorkspaceReady", "True", v1alpha1.ReasonSetupComplete)

	// Case 4: The runner cannot be reached, so the task is never pushed -> the
	// resume fails with a transient error and WorkspaceReady is left alone.
	t.Setenv("ATENET_ROUTER_ADDR", "127.0.0.1:1")
	err = manager.Resume(ctx, task, nil, nil)
	if err == nil || !substrate.IsTransientError(err) {
		t.Fatalf("Resume with an unreachable runner = %v, want a transient error", err)
	}
	assertCondition(t, task, "Ready", "False", v1alpha1.ReasonSubstrateUnavailable)
	assertCondition(t, task, "WorkspaceReady", "True", v1alpha1.ReasonSetupComplete)
	t.Setenv("ATENET_ROUTER_ADDR", runnerSrv.URL)
	if got := len(mockSrv.actorTemplates); got != 1 {
		t.Fatalf("readiness updates and suspend/resume created %d templates, want 1", got)
	}

	// A command-only change reuses the same profile template (the task is
	// pushed to the runner on resume), while an image change creates a new one.
	task.Spec.Commands = []string{"python3", "agent.py"}
	if err := manager.Resume(ctx, task, nil, nil); err != nil {
		t.Fatalf("Resume with changed command failed: %v", err)
	}
	if got := len(mockSrv.actorTemplates); got != 1 {
		t.Errorf("command change left %d templates, want 1", got)
	}
	task.Spec.Profile = &v1alpha1.Profile{Image: "example.com/other:v2"}
	if err := manager.Resume(ctx, task, nil, nil); err != nil {
		t.Fatalf("Resume with changed image failed: %v", err)
	}
	if got := len(mockSrv.actorTemplates); got != 2 {
		t.Errorf("image change left %d templates, want 2", got)
	}
}

// Resume reports what the runner does with the pushed task: a rejected push
// fails the resume, a setup failure the runner reports marks the workspace
// SetupFailed, and a failure it reports once the workspace is set up fails the
// resume.
func TestTaskManager_ResumeTaskPush(t *testing.T) {
	ctx := context.Background()

	var pushCode, checkCode atomic.Int32
	var pushed atomic.Value
	httpMux := http.NewServeMux()
	httpMux.HandleFunc("POST "+v1alpha1.RunnerTaskPath, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		pushed.Store(string(b))
		http.Error(w, "", int(pushCode.Load()))
	})
	httpMux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "git clone failed", int(checkCode.Load()))
	})
	runnerSrv := httptest.NewServer(httpMux)
	defer runnerSrv.Close()
	t.Setenv("ATENET_ROUTER_ADDR", runnerSrv.URL)

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

	client, err := substrate.NewClientWithOptions(substrate.ClientOptions{Target: lis.Addr().String(), Plaintext: true})
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()
	manager := controller.NewTaskManager(client)
	manager.WorkspaceReadyTimeout = 2 * time.Second

	newTask := func(name string) *v1alpha1.Task {
		return &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: name, Atespace: "default", Uid: name + "-uid"},
			Spec: &v1alpha1.TaskSpec{
				Profile:   &v1alpha1.Profile{Image: "ghcr.io/agent@sha256:a"},
				Workspace: &v1alpha1.WorkspaceRef{Name: "repo"},
				Commands:  []string{"python3", "agent.py"},
			},
		}
	}
	ws := &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "repo", Atespace: "default"}, Spec: &v1alpha1.WorkspaceSpec{}}

	// The runner holds another task: Resume fails.
	pushCode.Store(http.StatusConflict)
	task := newTask("rejected")
	if err := manager.Resume(ctx, task, ws, nil); err == nil {
		t.Fatal("Resume succeeded although the runner rejected the task")
	}
	assertCondition(t, task, "Ready", "False", v1alpha1.ReasonActorResumeFailed)
	body, _ := pushed.Load().(string)
	if !strings.Contains(body, "name: rejected") || !strings.Contains(body, "agent.py") || !strings.Contains(body, "---") || !strings.Contains(body, "name: repo") {
		t.Errorf("pushed body %q lacks the task and its workspace", body)
	}

	// The runner accepts the task but setup fails.
	pushCode.Store(http.StatusAccepted)
	checkCode.Store(http.StatusInternalServerError)
	task = newTask("setup-fails")
	if err := manager.Resume(ctx, task, ws, nil); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	assertCondition(t, task, "WorkspaceReady", "False", v1alpha1.WorkspaceReasonSetupFailed)
	if got := conditionMessage(task, "WorkspaceReady"); !strings.Contains(got, "git clone failed") {
		t.Errorf("WorkspaceReady message = %q, want the runner's reason", got)
	}

	// The task starts, is suspended, and cannot be started on the next
	// resume: the workspace is ready, so the resume fails.
	checkCode.Store(http.StatusOK)
	task = newTask("restart-fails")
	if err := manager.Resume(ctx, task, ws, nil); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	assertCondition(t, task, "WorkspaceReady", "True", v1alpha1.ReasonSetupComplete)
	checkCode.Store(http.StatusInternalServerError)
	err = manager.Resume(ctx, task, ws, nil)
	if err == nil || !strings.Contains(err.Error(), "git clone failed") {
		t.Fatalf("Resume = %v, want the runner's reason for not starting the task", err)
	}
	if substrate.IsTransientError(err) {
		t.Errorf("Resume error %v is transient; a task that cannot start does not start on a retry", err)
	}
	assertCondition(t, task, "Ready", "False", v1alpha1.ReasonActorResumeFailed)
	assertCondition(t, task, "WorkspaceReady", "True", v1alpha1.ReasonSetupComplete)
}

// startPushRunner runs the real runner, waiting for a push, with its state
// under a temp dir, and returns the address of its probe port.
func startPushRunner(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	origAXDir, origWSPath := workspace.AXDir, runner.DefaultWorkspacePath
	workspace.AXDir, runner.DefaultWorkspacePath = filepath.Join(dir, "ax"), filepath.Join(dir, "workspace")
	t.Cleanup(func() { workspace.AXDir, runner.DefaultWorkspacePath = origAXDir, origWSPath })

	freePort := func() int {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("finding a free port: %v", err)
		}
		defer lis.Close()
		return lis.Addr().(*net.TCPAddr).Port
	}
	port, noDrop := freePort(), false
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runner.Run(ctx, runner.Config{
			Port:           port,
			MetadataPort:   freePort(),
			RunDir:         filepath.Join(dir, "run"),
			AwaitPush:      true,
			DropPrivileges: &noDrop,
		})
	}()
	// Registered after the cleanup above, so the runner has stopped before
	// the directories it uses are restored.
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("runner.Run: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("runner did not stop")
		}
	})
	return net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
}

// A task whose command cannot be started (a typo in spec.commands) must not
// resume as if it were running. These cases resume against the real runner, so
// they also cover what it reports for such a task.
func TestTaskManager_ResumeCommandCannotStart(t *testing.T) {
	// resume resumes a task with the given command on a fresh runner. A
	// prewarmed task starts from a workspace snapshot, so its workspace is
	// ready before its first resume.
	resume := func(t *testing.T, commands []string, prewarmed bool) (*v1alpha1.Task, error) {
		t.Helper()
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("failed to listen: %v", err)
		}
		t.Setenv("ATENET_ROUTER_ADDR", startPushRunner(t))
		grpcServer := grpc.NewServer()
		ateapipb.RegisterControlServer(grpcServer, &mockControlServer{})
		go grpcServer.Serve(lis)
		t.Cleanup(grpcServer.Stop)
		client, err := substrate.NewClientWithOptions(substrate.ClientOptions{Target: lis.Addr().String(), Plaintext: true})
		if err != nil {
			t.Fatalf("failed to create substrate client: %v", err)
		}
		t.Cleanup(func() { client.Close() })
		manager := controller.NewTaskManager(client)
		manager.WorkspaceReadyTimeout = 10 * time.Second

		task := &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "task", Atespace: "default", Uid: "task-uid"},
			Spec: &v1alpha1.TaskSpec{
				Profile:  &v1alpha1.Profile{Image: "ghcr.io/agent@sha256:a"},
				Commands: commands,
			},
		}
		if prewarmed {
			task.Status = &v1alpha1.TaskStatus{Conditions: []*v1alpha1.Condition{
				{Type: v1alpha1.ConditionWorkspaceReady, Status: "True", Reason: v1alpha1.ReasonSetupComplete},
			}}
		}
		return task, manager.Resume(context.Background(), task, nil, nil)
	}
	typo := []string{"ax-no-such-command", "agent.py"}

	t.Run("a command that starts resumes", func(t *testing.T) {
		task, err := resume(t, []string{"sleep", "30"}, true)
		if err != nil {
			t.Fatalf("Resume failed: %v", err)
		}
		assertCondition(t, task, "WorkspaceReady", "True", v1alpha1.ReasonSetupComplete)
	})

	t.Run("a typo fails the resume of a task whose workspace is ready", func(t *testing.T) {
		task, err := resume(t, typo, true)
		if err == nil {
			t.Fatal("Resume succeeded although the task's command cannot be started")
		}
		if !strings.Contains(err.Error(), "ax-no-such-command") {
			t.Errorf("Resume error = %v, want it to name the command", err)
		}
		// The workspace is ready, so the error must not blame its setup.
		if strings.Contains(err.Error(), "workspace setup failed") {
			t.Errorf("Resume error = %v, want no mention of workspace setup", err)
		}
		if substrate.IsTransientError(err) {
			t.Errorf("Resume error %v is transient; the command does not start on a retry", err)
		}
		assertCondition(t, task, "Ready", "False", v1alpha1.ReasonActorResumeFailed)
		if got := conditionMessage(task, "Ready"); !strings.Contains(got, "ax-no-such-command") || strings.Contains(got, "workspace setup failed") {
			t.Errorf("Ready message = %q, want it to name the command and not workspace setup", got)
		}
	})

	// Before the workspace is known to be ready, the one check the runner
	// serves cannot tell a failed setup from a command that did not start, so
	// the resume returns and the task is reported as not ready, with the
	// runner's reason.
	t.Run("a typo on the first resume leaves the task not ready", func(t *testing.T) {
		task, err := resume(t, typo, false)
		if err != nil {
			t.Fatalf("Resume failed: %v", err)
		}
		assertCondition(t, task, "WorkspaceReady", "False", v1alpha1.WorkspaceReasonSetupFailed)
		if got := conditionMessage(task, "WorkspaceReady"); !strings.Contains(got, "ax-no-such-command") {
			t.Errorf("WorkspaceReady message = %q, want it to name the command", got)
		}
	})
}

func TestTaskManager_ResourceLimits(t *testing.T) {
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

	client, err := substrate.NewClientWithOptions(substrate.ClientOptions{Target: lis.Addr().String(), Plaintext: true})
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	manager := controller.NewTaskManager(client)
	manager.WorkspaceReadyTimeout = 200 * time.Millisecond

	task := &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata: &v1alpha1.ObjectMeta{
			Name:     "sized-task",
			Atespace: "default",
		},
		Spec: &v1alpha1.TaskSpec{
			Profile: &v1alpha1.Profile{
				Image: "ghrc.io/my-org/my-image",
				Resources: &v1alpha1.ResourcesSpec{
					Limits: &v1alpha1.ResourceList{Cpu: "2", Memory: "4Gi"},
				},
			},
		},
	}

	if err := manager.Suspend(ctx, task, nil, nil); err != nil {
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
	task.Spec.Profile.Resources.Limits.Memory = "8Gi"
	if err := manager.Suspend(ctx, task, nil, nil); err != nil {
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
		Spec:       &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "ghrc.io/my-org/my-image"}},
	}
	if err := manager.Suspend(ctx, plain, nil, nil); err != nil {
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
func TestTaskManager_ResourceLimitsFailurePaths(t *testing.T) {
	ctx := context.Background()

	newTask := func(name string, resources *v1alpha1.ResourcesSpec) *v1alpha1.Task {
		return &v1alpha1.Task{
			ApiVersion: v1alpha1.APIVersion,
			Kind:       v1alpha1.KindTask,
			Metadata:   &v1alpha1.ObjectMeta{Name: name, Atespace: "default"},
			Spec: &v1alpha1.TaskSpec{
				Profile: &v1alpha1.Profile{Image: "ghrc.io/my-org/my-image", Resources: resources},
			},
		}
	}
	setup := func(t *testing.T, mockSrv *mockControlServer) *controller.TaskManager {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("failed to listen: %v", err)
		}
		grpcServer := grpc.NewServer()
		ateapipb.RegisterControlServer(grpcServer, mockSrv)
		go grpcServer.Serve(lis)
		client, err := substrate.NewClientWithOptions(substrate.ClientOptions{Target: lis.Addr().String(), Plaintext: true})
		if err != nil {
			t.Fatalf("failed to create substrate client: %v", err)
		}
		t.Cleanup(func() {
			client.Close()
			grpcServer.Stop()
			lis.Close()
		})
		manager := controller.NewTaskManager(client)
		manager.WorkspaceReadyTimeout = 200 * time.Millisecond
		return manager
	}

	t.Run("invalid limits fail before provisioning", func(t *testing.T) {
		mockSrv := &mockControlServer{}
		manager := setup(t, mockSrv)

		for _, reqs := range []*v1alpha1.ResourcesSpec{
			{Limits: &v1alpha1.ResourceList{Cpu: "two"}},
			{Limits: &v1alpha1.ResourceList{Cpu: "0"}},
			{Limits: &v1alpha1.ResourceList{Cpu: "1000"}},
			{Limits: &v1alpha1.ResourceList{Memory: "-4Gi"}},
		} {
			task := newTask("bad-limits", reqs)
			if err := manager.Suspend(ctx, task, nil, nil); err == nil {
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
		manager := setup(t, mockSrv)

		task := newTask("rejected-limits", &v1alpha1.ResourcesSpec{
			Limits: &v1alpha1.ResourceList{Cpu: "2", Memory: "4Gi"},
		})
		if err := manager.Suspend(ctx, task, nil, nil); err == nil {
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
		manager := setup(t, mockSrv)

		task := newTask("no-limits", nil)
		if err := manager.Suspend(ctx, task, nil, nil); err == nil {
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

	client, err := substrate.NewClientWithOptions(substrate.ClientOptions{Target: lis.Addr().String(), Plaintext: true})
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	manager := controller.NewTaskManager(client)
	manager.WorkspaceReadyTimeout = 200 * time.Millisecond

	if err := manager.Delete(ctx, "default", "job"); err != nil {
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

	client, err := substrate.NewClientWithOptions(substrate.ClientOptions{Target: lis.Addr().String(), Plaintext: true})
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	manager := controller.NewTaskManager(client)

	if err := manager.Delete(ctx, "default", "slow-delete-task"); err != nil {
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

	client, err := substrate.NewClientWithOptions(substrate.ClientOptions{Target: lis.Addr().String(), Plaintext: true})
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	manager := controller.NewTaskManager(client)

	err = manager.Delete(ctx, "default", "stuck-task")
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

	client, err := substrate.NewClientWithOptions(substrate.ClientOptions{Target: lis.Addr().String(), Plaintext: true})
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

func TestTaskManager_TransientSubstrateUnavailable(t *testing.T) {
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

	client, err := substrate.NewClientWithOptions(substrate.ClientOptions{Target: lis.Addr().String(), Plaintext: true})
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	manager := controller.NewTaskManager(client)
	manager.WorkspaceReadyTimeout = 50 * time.Millisecond

	// 1. A transient error during EnsureAtespace is recorded with Reason="SubstrateUnavailable".
	mockSrv.createAtespaceErr = status.Error(codes.DeadlineExceeded, "substrate control plane timed out")
	task := &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "transient-atespace"},
		Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "ghrc.io/my-org/my-image"}},
	}
	if err := manager.Suspend(ctx, task, nil, nil); err == nil {
		t.Fatal("expected error from Suspend when EnsureAtespace fails, got nil")
	}
	assertCondition(t, task, "Ready", "False", v1alpha1.ReasonSubstrateUnavailable)
	mockSrv.createAtespaceErr = nil

	// 2. A transient error during EnsureActor is recorded with Reason="SubstrateUnavailable".
	mockSrv.createActorErr = status.Error(codes.DeadlineExceeded, "substrate actor creation timed out")
	task2 := &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "transient-actor"},
		Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "ghrc.io/my-org/my-image"}},
	}
	if err := manager.Suspend(ctx, task2, nil, nil); err == nil {
		t.Fatal("expected error from Suspend when EnsureActor fails, got nil")
	}
	assertCondition(t, task2, "Ready", "False", v1alpha1.ReasonSubstrateUnavailable)
	mockSrv.createActorErr = nil

	// 3. A transient error during ResumeActor is recorded with Reason="SubstrateUnavailable",
	// and a subsequent Resume once Substrate recovers resumes the actor.
	mockSrv.resumeActorErr = status.Error(codes.DeadlineExceeded, "substrate worker pool temporarily unreachable")
	task3 := &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "transient-resume"},
		Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "ghrc.io/my-org/my-image"}},
	}
	if err := manager.Resume(ctx, task3, nil, nil); err == nil {
		t.Fatal("expected error from Resume when ResumeActor fails, got nil")
	}
	assertCondition(t, task3, "Ready", "False", v1alpha1.ReasonSubstrateUnavailable)

	// Recovery on retry:
	mockSrv.resumeActorErr = nil
	if err := manager.Resume(ctx, task3, nil, nil); err != nil {
		t.Fatalf("expected Resume to succeed after Substrate recovers, got %v", err)
	}
	if len(mockSrv.resumedActors) != 1 || mockSrv.resumedActors[0] != "transient-resume" {
		t.Errorf("expected actor 'transient-resume' resumed after recovery, got %v", mockSrv.resumedActors)
	}

	// 4. A permanent error is recorded with its own reason.
	mockSrv.createActorErr = status.Error(codes.InvalidArgument, "invalid actor specification")
	task4 := &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "permanent-actor-err"},
		Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "ghrc.io/my-org/my-image"}},
	}
	if err := manager.Suspend(ctx, task4, nil, nil); err == nil {
		t.Fatal("expected error on InvalidArgument, got nil")
	}
	assertCondition(t, task4, "Ready", "False", v1alpha1.ReasonActorCreationFailed)
	mockSrv.createActorErr = nil
}

func TestTaskManager_ATETemplate(t *testing.T) {
	ctx := context.Background()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	mockSrv := &mockControlServer{actorTemplates: map[string]bool{"custom-base": true}}
	grpcServer := grpc.NewServer()
	ateapipb.RegisterControlServer(grpcServer, mockSrv)
	go grpcServer.Serve(lis)
	defer grpcServer.Stop()

	client, err := substrate.NewClientWithOptions(substrate.ClientOptions{Target: lis.Addr().String(), Plaintext: true})
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	manager := controller.NewTaskManager(client)
	manager.WorkspaceReadyTimeout = 50 * time.Millisecond

	task := &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "tpl-task", Atespace: "default"},
		Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{AteTemplate: &v1alpha1.ObjectRef{Name: "custom-base"}}},
	}
	if err := manager.Suspend(ctx, task, nil, nil); err != nil {
		t.Fatalf("Suspend failed: %v", err)
	}
	if got := task.GetStatus().GetAteTemplate(); got.GetName() != "custom-base" || got.GetAtespace() != "default" {
		t.Errorf("AteTemplate = %v, want {Name: custom-base, Atespace: default}", got)
	}
	if len(mockSrv.createdTemplates) != 0 {
		t.Errorf("expected no per-task template created, got %d", len(mockSrv.createdTemplates))
	}
	if err := manager.DeleteTask(ctx, task); err != nil {
		t.Fatalf("DeleteTask failed: %v", err)
	}
	if !mockSrv.actorTemplates["custom-base"] {
		t.Errorf("user-supplied template custom-base should not be deleted by DeleteTask")
	}

	// ateTemplate.atespace looks the template up, and binds the actor to it, in that
	// atespace rather than the task's own.
	mockSrv.templateRefs, mockSrv.actorTemplateRefs = nil, nil
	crossTask := &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "cross-tpl-task", Atespace: "default"},
		Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{AteTemplate: &v1alpha1.ObjectRef{Name: "custom-base", Atespace: "shared"}}},
	}
	if err := manager.Suspend(ctx, crossTask, nil, nil); err != nil {
		t.Fatalf("Suspend with ateTemplate.atespace failed: %v", err)
	}
	wantRef := &ateapipb.ObjectRef{Atespace: "shared", Name: "custom-base"}
	if len(mockSrv.templateRefs) != 1 || !proto.Equal(mockSrv.templateRefs[0], wantRef) {
		t.Errorf("GetActorTemplate refs = %v, want [%v]", mockSrv.templateRefs, wantRef)
	}
	if len(mockSrv.actorTemplateRefs) != 1 || !proto.Equal(mockSrv.actorTemplateRefs[0], wantRef) {
		t.Errorf("CreateActor template refs = %v, want [%v]", mockSrv.actorTemplateRefs, wantRef)
	}
	if got := crossTask.GetStatus().GetAteTemplate(); got.GetName() != "custom-base" || got.GetAtespace() != "shared" {
		t.Errorf("crossTask AteTemplate = %v, want {Name: custom-base, Atespace: shared}", got)
	}
}

func TestTaskManager_WorkspaceProfilePrecedence(t *testing.T) {
	ctx := context.Background()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	mockSrv := &mockControlServer{actorTemplates: map[string]bool{"ws-base-tpl": true}}
	grpcServer := grpc.NewServer()
	ateapipb.RegisterControlServer(grpcServer, mockSrv)
	go grpcServer.Serve(lis)
	defer grpcServer.Stop()

	client, err := substrate.NewClientWithOptions(substrate.ClientOptions{Target: lis.Addr().String(), Plaintext: true})
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer client.Close()

	const taskImg = "ghcr.io/task/image@sha256:1111"
	manager := controller.NewTaskManager(client)
	manager.WorkspaceReadyTimeout = 50 * time.Millisecond

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "warm-ws", Atespace: "default"},
		Spec: &v1alpha1.WorkspaceSpec{
			Profile: &v1alpha1.Profile{
				Image: "ghcr.io/ws/image@sha256:2222",
				Resources: &v1alpha1.ResourcesSpec{
					Limits: &v1alpha1.ResourceList{Cpu: "4", Memory: "8Gi"},
				},
			},
		},
	}

	// 1. Omitted task profile falls back to Workspace.spec.profile.
	inheritedTask := &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "inherited-task", Atespace: "default"},
		Spec:     &v1alpha1.TaskSpec{Workspace: &v1alpha1.WorkspaceRef{Name: "warm-ws"}},
	}
	if err := manager.Suspend(ctx, inheritedTask, ws, nil); err != nil {
		t.Fatalf("Suspend(inheritedTask) failed: %v", err)
	}
	if len(mockSrv.createdTemplates) != 1 {
		t.Fatalf("createdTemplates = %d, want 1", len(mockSrv.createdTemplates))
	}
	gotInherited := mockSrv.createdTemplates[0]
	if got := gotInherited.GetContainers()[0].GetImage(); got != "ghcr.io/ws/image@sha256:2222" {
		t.Errorf("inherited image = %q, want ghcr.io/ws/image@sha256:2222", got)
	}
	wantWsRes := &ateapipb.ResourceRequirements{Limits: &ateapipb.Resources{
		CpuMillicores: 4000,
		MemoryBytes:   8 << 30,
	}}
	if !proto.Equal(gotInherited.GetResources(), wantWsRes) {
		t.Errorf("inherited resources = %v, want %v", gotInherited.GetResources(), wantWsRes)
	}

	// 2. A task profile is used as-is: unset CPU falls back to the default,
	// NOT Workspace.spec.profile.
	resourcesOnlyTask := &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "resources-only-task", Atespace: "default"},
		Spec: &v1alpha1.TaskSpec{
			Workspace: &v1alpha1.WorkspaceRef{Name: "warm-ws"},
			Profile: &v1alpha1.Profile{
				Image: taskImg,
				Resources: &v1alpha1.ResourcesSpec{
					Limits: &v1alpha1.ResourceList{Memory: "2Gi"},
				},
			},
		},
	}
	if err := manager.Suspend(ctx, resourcesOnlyTask, ws, nil); err != nil {
		t.Fatalf("Suspend(resourcesOnlyTask) failed: %v", err)
	}
	if len(mockSrv.createdTemplates) != 2 {
		t.Fatalf("createdTemplates = %d, want 2", len(mockSrv.createdTemplates))
	}
	gotResOnly := mockSrv.createdTemplates[1]
	if got := gotResOnly.GetContainers()[0].GetImage(); got != taskImg {
		t.Errorf("resources-only task image = %q, want %q", got, taskImg)
	}
	wantTaskRes := &ateapipb.ResourceRequirements{Limits: &ateapipb.Resources{
		CpuMillicores: substrate.DefaultCPUMillicores,
		MemoryBytes:   2 << 30,
	}}
	if !proto.Equal(gotResOnly.GetResources(), wantTaskRes) {
		t.Errorf("resources-only task resources = %v, want %v", gotResOnly.GetResources(), wantTaskRes)
	}

	// 3. Even when Workspace uses ateTemplate.name, a task setting its own
	// profile uses it cleanly without conflicting with Workspace.spec.profile.
	wsWithTpl := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "tpl-ws", Atespace: "default"},
		Spec:     &v1alpha1.WorkspaceSpec{Profile: &v1alpha1.Profile{AteTemplate: &v1alpha1.ObjectRef{Name: "ws-base-tpl"}}},
	}
	taskOverTplWs := &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "task-over-tpl-ws", Atespace: "default"},
		Spec: &v1alpha1.TaskSpec{
			Workspace: &v1alpha1.WorkspaceRef{Name: "tpl-ws"},
			Profile: &v1alpha1.Profile{
				Image: taskImg,
				Resources: &v1alpha1.ResourcesSpec{
					Limits: &v1alpha1.ResourceList{Memory: "1Gi"},
				},
			},
		},
	}
	if err := manager.Suspend(ctx, taskOverTplWs, wsWithTpl, nil); err != nil {
		t.Fatalf("Suspend(taskOverTplWs) failed: %v", err)
	}
	if len(mockSrv.createdTemplates) != 3 {
		t.Fatalf("createdTemplates = %d, want 3", len(mockSrv.createdTemplates))
	}
	if got := mockSrv.createdTemplates[2].GetContainers()[0].GetImage(); got != taskImg {
		t.Errorf("taskOverTplWs image = %q, want %q", got, taskImg)
	}
}

// conditionMessage returns the message of the task's condition of type condType.
func conditionMessage(task *v1alpha1.Task, condType string) string {
	for _, c := range task.GetStatus().GetConditions() {
		if c.GetType() == condType {
			return c.GetMessage()
		}
	}
	return ""
}
