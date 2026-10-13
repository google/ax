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

package substrate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type mockControlClient struct {
	ateapipb.ControlClient

	mu                  sync.Mutex
	deleteCalls         []*ateapipb.DeleteActorRequest
	getCalls            []*ateapipb.GetActorRequest
	deleteErr           error
	actorRemaining      int // Number of GetActor calls before returning NotFound
	resumeErrs          []error
	resumeActor         *ateapipb.Actor
	deleteTemplateErrs  []error
	deleteTemplateCalls int
	templates           map[string]*ateapipb.ActorTemplate
	createTemplateErrs  []error
	createdTemplates    []*ateapipb.ActorTemplate
}

func (m *mockControlClient) GetActorTemplate(ctx context.Context, req *ateapipb.GetActorTemplateRequest, opts ...grpc.CallOption) (*ateapipb.ActorTemplate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := req.GetActorTemplate().GetAtespace() + "/" + req.GetActorTemplate().GetName()
	if tmpl, ok := m.templates[key]; ok {
		return tmpl, nil
	}
	return nil, status.Error(codes.NotFound, "template not found")
}

func (m *mockControlClient) CreateActorTemplate(ctx context.Context, req *ateapipb.CreateActorTemplateRequest, opts ...grpc.CallOption) (*ateapipb.ActorTemplate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.createTemplateErrs) > 0 {
		err := m.createTemplateErrs[0]
		m.createTemplateErrs = m.createTemplateErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	m.createdTemplates = append(m.createdTemplates, req.GetActorTemplate())
	return req.GetActorTemplate(), nil
}

func (m *mockControlClient) ResumeActor(ctx context.Context, req *ateapipb.ResumeActorRequest, opts ...grpc.CallOption) (*ateapipb.ResumeActorResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.resumeErrs) > 0 {
		err := m.resumeErrs[0]
		m.resumeErrs = m.resumeErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	return &ateapipb.ResumeActorResponse{Actor: m.resumeActor, Resumed: true}, nil
}

func (m *mockControlClient) DeleteActorTemplate(ctx context.Context, req *ateapipb.DeleteActorTemplateRequest, opts ...grpc.CallOption) (*ateapipb.ActorTemplate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteTemplateCalls++
	if len(m.deleteTemplateErrs) > 0 {
		err := m.deleteTemplateErrs[0]
		m.deleteTemplateErrs = m.deleteTemplateErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	return &ateapipb.ActorTemplate{}, nil
}

func (m *mockControlClient) DeleteActor(ctx context.Context, req *ateapipb.DeleteActorRequest, opts ...grpc.CallOption) (*ateapipb.Actor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteCalls = append(m.deleteCalls, req)
	if m.deleteErr != nil {
		return nil, m.deleteErr
	}
	return &ateapipb.Actor{}, nil
}

func (m *mockControlClient) GetActor(ctx context.Context, req *ateapipb.GetActorRequest, opts ...grpc.CallOption) (*ateapipb.Actor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCalls = append(m.getCalls, req)
	if m.actorRemaining > 0 {
		m.actorRemaining--
		return &ateapipb.Actor{
			Metadata: &ateapipb.ResourceMetadata{
				Atespace: req.GetActor().GetAtespace(),
				Name:     req.GetActor().GetName(),
			},
			Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_DELETING},
		}, nil
	}
	return nil, status.Error(codes.NotFound, "actor not found")
}

func TestClient_DeleteActor_AnyStateTrue(t *testing.T) {
	mock := &mockControlClient{
		actorRemaining: 0,
	}
	c := &Client{control: mock}
	ctx := context.Background()

	err := c.DeleteActor(ctx, "test-ns", "test-actor")
	if err != nil {
		t.Fatalf("DeleteActor failed: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.deleteCalls) != 1 {
		t.Fatalf("expected 1 DeleteActor call, got %d", len(mock.deleteCalls))
	}
	req := mock.deleteCalls[0]
	if !req.GetAnyState() {
		t.Errorf("expected AnyState to be true, got %v", req.GetAnyState())
	}
	if req.GetActor().GetAtespace() != "test-ns" || req.GetActor().GetName() != "test-actor" {
		t.Errorf("unexpected actor ref in request: %v", req.GetActor())
	}
}

func TestClient_DeleteActor_PollsUntilNotFound(t *testing.T) {
	oldInterval := defaultRetryInterval
	defaultRetryInterval = 10 * time.Millisecond
	defer func() { defaultRetryInterval = oldInterval }()

	mock := &mockControlClient{
		actorRemaining: 3,
	}
	c := &Client{control: mock}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := c.DeleteActor(ctx, "test-ns", "test-actor")
	if err != nil {
		t.Fatalf("DeleteActor failed: %v", err)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	// 1 fast-path GetActor call + 3 polling calls (last one returns NotFound)
	if len(mock.getCalls) != 4 {
		t.Errorf("expected 4 GetActor calls before NotFound, got %d", len(mock.getCalls))
	}
}

func TestClient_DeleteActor_NotFoundIsIgnored(t *testing.T) {
	mock := &mockControlClient{
		deleteErr: status.Error(codes.NotFound, "actor already gone"),
	}
	c := &Client{control: mock}
	ctx := context.Background()

	err := c.DeleteActor(ctx, "test-ns", "test-actor")
	if err != nil {
		t.Fatalf("expected nil error when actor is NotFound, got: %v", err)
	}
}

func TestClient_DeleteActor_PropagatesRPCError(t *testing.T) {
	mock := &mockControlClient{
		deleteErr: errors.New("internal atelet failure"),
	}
	c := &Client{control: mock}
	ctx := context.Background()

	err := c.DeleteActor(ctx, "test-ns", "test-actor")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestBuildActorTemplate(t *testing.T) {
	t.Setenv("AX_SNAPSHOTS_BUCKET", "gs://ate-snapshots/ax/")

	tmpl := BuildActorTemplate("", "test-tmpl", "", map[string]string{"FOO": "BAR"}, nil, "")
	if tmpl.GetMetadata().GetAtespace() != "default" {
		t.Errorf("expected default atespace, got %q", tmpl.GetMetadata().GetAtespace())
	}
	if len(tmpl.GetContainers()) != 1 {
		t.Fatalf("expected 1 container, got %d", len(tmpl.GetContainers()))
	}
	container := tmpl.GetContainers()[0]
	if container.GetImage() != v1alpha1.DefaultTaskImage {
		t.Errorf("unexpected image: %q", container.GetImage())
	}
	probe := container.GetWakeupProbe().GetHttpGet()
	if probe == nil || probe.GetPath() != "/readyz" || probe.GetPort() != 80 {
		t.Errorf("unexpected wakeupProbe: %+v", container.GetWakeupProbe())
	}
	snapCfg := tmpl.GetSnapshotConfig()
	if snapCfg == nil {
		t.Fatal("expected SnapshotConfig to be set")
	}
	if snapCfg.GetStorageLocation() != "gs://ate-snapshots/ax/" {
		t.Errorf("expected StorageLocation gs://ate-snapshots/ax/, got %q", snapCfg.GetStorageLocation())
	}
	if snapCfg.GetPreferredFidelity() != ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES {
		t.Errorf("expected PreferredFidelity VOLUMES, got %v", snapCfg.GetPreferredFidelity())
	}
}

func TestClient_ResumeActor_WorkerPodIpsAndRetry(t *testing.T) {
	oldInterval := defaultRetryInterval
	defaultRetryInterval = 5 * time.Millisecond
	defer func() { defaultRetryInterval = oldInterval }()

	mock := &mockControlClient{
		resumeErrs: []error{
			status.Error(codes.ResourceExhausted, "no worker available"),
			nil,
		},
		resumeActor: &ateapipb.Actor{
			Metadata: &ateapipb.ResourceMetadata{Atespace: "default", Name: "task-1"},
			Status: &ateapipb.ActorStatus{
				State: ateapipb.ActorState_ACTOR_STATE_RUNNING,
				WorkerAssignment: &ateapipb.WorkerAssignment{
					// The first (primary) IP is used, even when it is IPv6.
					WorkerPodIps: []string{"fd00::99", "10.244.0.99"},
				},
			},
		},
	}
	c := &Client{control: mock}
	actor, ip, err := c.ResumeActor(context.Background(), "default", "task-1")
	if err != nil {
		t.Fatalf("ResumeActor failed: %v", err)
	}
	if actor == nil {
		t.Fatal("expected non-nil actor")
	}
	if ip != "fd00::99" {
		t.Errorf("expected primary worker IP fd00::99, got %q", ip)
	}
}

func TestClient_DeleteActorTemplate_RetriesOnAborted(t *testing.T) {
	oldInterval := defaultRetryInterval
	defaultRetryInterval = 5 * time.Millisecond
	defer func() { defaultRetryInterval = oldInterval }()

	mock := &mockControlClient{
		deleteTemplateErrs: []error{
			status.Error(codes.Aborted, "another operation is in progress for this ActorTemplate"),
			status.Error(codes.Aborted, "another operation is in progress for this ActorTemplate"),
			nil,
		},
	}
	c := &Client{control: mock}
	if err := c.DeleteActorTemplate(context.Background(), "default", "tmpl-1"); err != nil {
		t.Fatalf("DeleteActorTemplate failed: %v", err)
	}
	if mock.deleteTemplateCalls != 3 {
		t.Errorf("expected 3 DeleteActorTemplate calls, got %d", mock.deleteTemplateCalls)
	}
}

func TestClient_EnsureActorTemplateWithImage_FallbackToBaseTemplate(t *testing.T) {
	baseTmpl := &ateapipb.ActorTemplate{
		Metadata: &ateapipb.ResourceMetadata{
			Atespace: v1alpha1.DefaultSystemNamespace,
			Name:     "default-template",
		},
		SnapshotConfig: &ateapipb.SnapshotConfig{
			StorageLocation:   "gs://cluster-base-bucket/snapshots/",
			PreferredFidelity: ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY,
		},
	}
	mock := &mockControlClient{
		templates: map[string]*ateapipb.ActorTemplate{
			v1alpha1.DefaultSystemNamespace + "/default-template": baseTmpl,
		},
		createTemplateErrs: []error{
			status.Error(codes.InvalidArgument, "actor_template.snapshot_config: unknown field"),
			nil,
		},
	}
	c := &Client{control: mock}

	created, err := c.EnsureActorTemplateWithImage(
		context.Background(),
		v1alpha1.DefaultSystemNamespace,
		"default-template",
		"default",
		"task-1-tmpl-abcd",
		"custom-image:v1",
		map[string]string{"FOO": "BAR"},
	)
	if err != nil {
		t.Fatalf("EnsureActorTemplateWithImage failed: %v", err)
	}
	if created.GetMetadata().GetAtespace() != "default" || created.GetMetadata().GetName() != "task-1-tmpl-abcd" {
		t.Errorf("unexpected metadata on fallback template: %+v", created.GetMetadata())
	}
	if created.GetSnapshotConfig().GetStorageLocation() != "gs://cluster-base-bucket/snapshots/" {
		t.Errorf("expected SnapshotConfig cloned from base template, got %+v", created.GetSnapshotConfig())
	}
	if len(created.GetContainers()) != 1 || created.GetContainers()[0].GetImage() != "custom-image:v1" {
		t.Errorf("expected container image custom-image:v1, got %+v", created.GetContainers())
	}

	// Verify what was actually sent to the server on the fallback attempt.
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.createdTemplates) != 1 {
		t.Fatalf("expected 1 successful CreateActorTemplate request, got %d", len(mock.createdTemplates))
	}
	sent := mock.createdTemplates[0]
	if sent.GetMetadata().GetAtespace() != "default" || sent.GetMetadata().GetName() != "task-1-tmpl-abcd" {
		t.Errorf("unexpected metadata in request: %+v", sent.GetMetadata())
	}
	if sent.GetSnapshotConfig().GetStorageLocation() != "gs://cluster-base-bucket/snapshots/" ||
		sent.GetSnapshotConfig().GetPreferredFidelity() != ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY {
		t.Errorf("expected SnapshotConfig cloned from base in request, got %+v", sent.GetSnapshotConfig())
	}
	if len(sent.GetVolumes()) == 0 || sent.GetSandboxConfig() == nil {
		t.Errorf("expected Volumes/SandboxConfig filled from desired template, got volumes=%+v sandbox=%+v", sent.GetVolumes(), sent.GetSandboxConfig())
	}
	if baseTmpl.GetMetadata().GetName() != "default-template" {
		t.Errorf("base template was mutated: %+v", baseTmpl.GetMetadata())
	}
}

func TestClient_EnsureActorTemplateWithImage_NoFallbackOnTransientError(t *testing.T) {
	mock := &mockControlClient{
		templates: map[string]*ateapipb.ActorTemplate{
			v1alpha1.DefaultSystemNamespace + "/default-template": {
				Metadata: &ateapipb.ResourceMetadata{Atespace: v1alpha1.DefaultSystemNamespace, Name: "default-template"},
			},
		},
		createTemplateErrs: []error{
			status.Error(codes.Unavailable, "connection refused"),
		},
	}
	c := &Client{control: mock}

	_, err := c.EnsureActorTemplateWithImage(context.Background(), v1alpha1.DefaultSystemNamespace, "default-template", "default", "task-1-tmpl-abcd", "custom-image:v1")
	if status.Code(errors.Unwrap(err)) != codes.Unavailable {
		t.Fatalf("expected Unavailable error, got %v", err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.createdTemplates) != 0 {
		t.Errorf("expected no fallback template creation, got %d", len(mock.createdTemplates))
	}
}

func TestRetryOnCodes(t *testing.T) {
	oldInterval, oldMax := defaultRetryInterval, maxRetryInterval
	defaultRetryInterval, maxRetryInterval = time.Millisecond, 2*time.Millisecond
	defer func() { defaultRetryInterval, maxRetryInterval = oldInterval, oldMax }()

	t.Run("non-retryable returns immediately", func(t *testing.T) {
		calls := 0
		wantErr := status.Error(codes.Internal, "boom")
		err := retryOnCodes(context.Background(), func() error {
			calls++
			return wantErr
		}, codes.Aborted)
		if err != wantErr || calls != 1 {
			t.Errorf("got err=%v (%T) calls=%d, want original Internal error after 1 call", err, err, calls)
		}
	})

	t.Run("non-retryable on final attempt is unwrapped", func(t *testing.T) {
		calls := 0
		wantErr := status.Error(codes.Internal, "boom")
		err := retryOnCodes(context.Background(), func() error {
			calls++
			if calls == defaultRetryAttempts {
				return wantErr
			}
			return status.Error(codes.Aborted, "busy")
		}, codes.Aborted)
		if err != wantErr || calls != defaultRetryAttempts {
			t.Errorf("got err=%v (%T) calls=%d, want original Internal error after %d calls", err, err, calls, defaultRetryAttempts)
		}
	})

	t.Run("exhausts attempts", func(t *testing.T) {
		calls := 0
		err := retryOnCodes(context.Background(), func() error {
			calls++
			return status.Error(codes.Aborted, "busy")
		}, codes.Aborted)
		if status.Code(err) != codes.Aborted || calls != defaultRetryAttempts {
			t.Errorf("got err=%v calls=%d, want Aborted after %d calls", err, calls, defaultRetryAttempts)
		}
	})

	t.Run("context cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := retryOnCodes(ctx, func() error {
			return status.Error(codes.Aborted, "busy")
		}, codes.Aborted)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	})
}
