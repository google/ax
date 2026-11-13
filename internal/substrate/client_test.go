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

	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
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
	createPolicyErr     error
	createActorErrs     []error
	createdActors       []*ateapipb.Actor
	tags                map[string]*ateapipb.Tag
	createTagErrs       []error
	deleteTagErrs       []error
	deleteTagCalls      int
}

func (m *mockControlClient) CreateActorEgressPolicy(ctx context.Context, req *ateapipb.CreateActorEgressPolicyRequest, opts ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createPolicyErr != nil {
		return nil, m.createPolicyErr
	}
	return req.GetEgressPolicy(), nil
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

func (m *mockControlClient) CreateActor(ctx context.Context, req *ateapipb.CreateActorRequest, opts ...grpc.CallOption) (*ateapipb.Actor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.createActorErrs) > 0 {
		err := m.createActorErrs[0]
		m.createActorErrs = m.createActorErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	m.createdActors = append(m.createdActors, req.GetActor())
	return req.GetActor(), nil
}

func (m *mockControlClient) CreateTag(ctx context.Context, req *ateapipb.CreateTagRequest, opts ...grpc.CallOption) (*ateapipb.Tag, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.createTagErrs) > 0 {
		err := m.createTagErrs[0]
		m.createTagErrs = m.createTagErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	if m.tags == nil {
		m.tags = make(map[string]*ateapipb.Tag)
	}
	tag := proto.Clone(req.GetTag()).(*ateapipb.Tag)
	tag.Status = &ateapipb.TagStatus{ActorTemplateUid: "tpl-uid-123"}
	m.tags[tag.GetMetadata().GetAtespace()+"/"+tag.GetMetadata().GetName()] = tag
	return proto.Clone(tag).(*ateapipb.Tag), nil
}

func (m *mockControlClient) GetTag(ctx context.Context, req *ateapipb.GetTagRequest, opts ...grpc.CallOption) (*ateapipb.Tag, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if tag, ok := m.tags[req.GetTag().GetAtespace()+"/"+req.GetTag().GetName()]; ok {
		return proto.Clone(tag).(*ateapipb.Tag), nil
	}
	return nil, status.Error(codes.NotFound, "tag not found")
}

func (m *mockControlClient) ListTags(ctx context.Context, req *ateapipb.ListTagsRequest, opts ...grpc.CallOption) (*ateapipb.ListTagsResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	resp := &ateapipb.ListTagsResponse{}
	for _, tag := range m.tags {
		if req.GetAtespace() == "" || tag.GetMetadata().GetAtespace() == req.GetAtespace() {
			resp.Tags = append(resp.Tags, proto.Clone(tag).(*ateapipb.Tag))
		}
	}
	return resp, nil
}

func (m *mockControlClient) DeleteTag(ctx context.Context, req *ateapipb.DeleteTagRequest, opts ...grpc.CallOption) (*ateapipb.Tag, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteTagCalls++
	if len(m.deleteTagErrs) > 0 {
		err := m.deleteTagErrs[0]
		m.deleteTagErrs = m.deleteTagErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	key := req.GetTag().GetAtespace() + "/" + req.GetTag().GetName()
	tag, ok := m.tags[key]
	if !ok {
		return nil, status.Error(codes.NotFound, "tag not found")
	}
	delete(m.tags, key)
	return tag, nil
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

func TestClient_EnsureActorEgressPolicy_AlreadyExistsIsIgnored(t *testing.T) {
	mock := &mockControlClient{
		createPolicyErr: status.Error(codes.AlreadyExists, "EgressPolicy already exists"),
	}
	c := &Client{control: mock}
	ctx := context.Background()

	err := c.EnsureActorEgressPolicy(ctx, "test-ns", "test-actor", nil)
	if err != nil {
		t.Fatalf("expected nil error when the policy already exists, got: %v", err)
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
	tmpl := BuildActorTemplate("", "test-tmpl", "", map[string]string{"FOO": "BAR"}, nil, "", nil)
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
	if snapCfg.GetStorageLocation() != "" {
		t.Errorf("expected empty StorageLocation by default in Substrate v1.0+, got %q", snapCfg.GetStorageLocation())
	}
	if snapCfg.GetPreferredFidelity() != ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES {
		t.Errorf("expected PreferredFidelity VOLUMES, got %v", snapCfg.GetPreferredFidelity())
	}

	// Every task gets the trust bundle, mounted where the runner looks for it.
	trustMount, trustVolume := container.GetVolumeMounts()[1], tmpl.GetVolumes()[1]
	bundle := trustVolume.GetSystemInfo().GetDataSources()[0].GetTrustBundle()
	if trustMount.GetName() != trustVolume.GetName() || trustMount.GetMountPath()+"/"+bundle.GetPath() != v1alpha1.EgressTrustBundlePath {
		t.Errorf("trust bundle is mounted at %s/%s, want %s", trustMount.GetMountPath(), bundle.GetPath(), v1alpha1.EgressTrustBundlePath)
	}
	if names := bundle.GetNames(); len(names) != 2 || names[0] != "egress-mitm.ate.dev" || names[1] != "system-roots.ate.dev" {
		t.Errorf("unexpected trust bundles: %v", names)
	}

	// When snapshotsBucket is explicitly provided, StorageLocation is set.
	tmplWithBucket := BuildActorTemplate("", "test-tmpl", "", nil, nil, "gs://ate-snapshots/ax/", nil)
	if tmplWithBucket.GetSnapshotConfig().GetStorageLocation() != "gs://ate-snapshots/ax/" {
		t.Errorf("expected StorageLocation gs://ate-snapshots/ax/, got %q", tmplWithBucket.GetSnapshotConfig().GetStorageLocation())
	}
}

func TestClient_ResumeActor_WorkerPodIpsAndRetry(t *testing.T) {
	oldInterval := defaultRetryInterval
	defaultRetryInterval = 5 * time.Millisecond
	defer func() { defaultRetryInterval = oldInterval }()

	mock := &mockControlClient{
		resumeErrs: []error{
			status.Error(codes.Aborted, "operation aborted"),
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

func TestClient_ResumeActor_ResourceExhaustedNotRetried(t *testing.T) {
	mock := &mockControlClient{
		resumeErrs: []error{
			status.Error(codes.ResourceExhausted, "no worker available"),
		},
	}
	c := &Client{control: mock}
	_, _, err := c.ResumeActor(context.Background(), "default", "task-1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("expected ResourceExhausted error, got %v", err)
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

func TestClient_EnsureActorTemplateWithImage_Success(t *testing.T) {
	mock := &mockControlClient{}
	c := &Client{control: mock}

	created, err := c.EnsureActorTemplateWithImage(
		context.Background(),
		v1alpha1.DefaultSystemNamespace,
		"default-template",
		"default",
		"task-1-tmpl-abcd",
		"custom-image:v1",
		nil,
		map[string]string{"FOO": "BAR"},
	)
	if err != nil {
		t.Fatalf("EnsureActorTemplateWithImage failed: %v", err)
	}
	if created.GetMetadata().GetAtespace() != "default" || created.GetMetadata().GetName() != "task-1-tmpl-abcd" {
		t.Errorf("unexpected metadata: %+v", created.GetMetadata())
	}
	if len(created.GetContainers()) != 1 || created.GetContainers()[0].GetImage() != "custom-image:v1" {
		t.Errorf("expected container image custom-image:v1, got %+v", created.GetContainers())
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.createdTemplates) != 1 {
		t.Fatalf("expected 1 CreateActorTemplate request, got %d", len(mock.createdTemplates))
	}
}

func TestClient_EnsureActorTemplateWithImage_PropagatesError(t *testing.T) {
	mock := &mockControlClient{
		createTemplateErrs: []error{
			status.Error(codes.InvalidArgument, "actor_template.snapshot_config: invalid field"),
		},
	}
	c := &Client{control: mock}

	_, err := c.EnsureActorTemplateWithImage(
		context.Background(),
		v1alpha1.DefaultSystemNamespace,
		"default-template",
		"default",
		"task-1-tmpl-abcd",
		"custom-image:v1",
		nil,
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument error, got %v", err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.createdTemplates) != 0 {
		t.Errorf("expected no created templates on failure, got %d", len(mock.createdTemplates))
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

	t.Run("retries Unavailable and succeeds", func(t *testing.T) {
		calls := 0
		err := retryOnCodes(context.Background(), func() error {
			calls++
			if calls < 3 {
				return status.Error(codes.Unavailable, "ateapi rolling out")
			}
			return nil
		}, codes.Unavailable)
		if err != nil || calls != 3 {
			t.Errorf("got err=%v calls=%d, want nil after 3 calls", err, calls)
		}
	})
}

func TestIsTransientError(t *testing.T) {
	if IsTransientError(nil) {
		t.Error("expected nil error to be non-transient")
	}
	if !IsTransientError(status.Error(codes.Unavailable, "connection refused")) {
		t.Error("expected codes.Unavailable to be transient")
	}
	if !IsTransientError(status.Error(codes.DeadlineExceeded, "rpc deadline exceeded")) {
		t.Error("expected codes.DeadlineExceeded to be transient")
	}
	if !IsTransientError(context.DeadlineExceeded) {
		t.Error("expected context.DeadlineExceeded to be transient")
	}
	if IsTransientError(status.Error(codes.InvalidArgument, "bad spec")) {
		t.Error("expected codes.InvalidArgument to be non-transient")
	}
}

func TestResourceLimits(t *testing.T) {
	tests := []struct {
		name string
		reqs *v1alpha1.ResourcesSpec
		want *ateapipb.ResourceRequirements
	}{
		{name: "nil"},
		{name: "empty", reqs: &v1alpha1.ResourcesSpec{}},
		{name: "empty limits", reqs: &v1alpha1.ResourcesSpec{Limits: &v1alpha1.ResourceList{}}},
		{
			name: "cpu limit",
			reqs: &v1alpha1.ResourcesSpec{Limits: &v1alpha1.ResourceList{Cpu: "2"}},
			want: &ateapipb.ResourceRequirements{Limits: &ateapipb.Resources{CpuMillicores: 2000, MemoryBytes: DefaultMemoryBytes}},
		},
		{
			name: "memory limit",
			reqs: &v1alpha1.ResourcesSpec{Limits: &v1alpha1.ResourceList{Memory: "4Gi"}},
			want: &ateapipb.ResourceRequirements{Limits: &ateapipb.Resources{CpuMillicores: DefaultCPUMillicores, MemoryBytes: 4 << 30}},
		},
		{
			name: "cpu and memory limits",
			reqs: &v1alpha1.ResourcesSpec{
				Limits: &v1alpha1.ResourceList{Cpu: "2", Memory: "4Gi"},
			},
			want: &ateapipb.ResourceRequirements{Limits: &ateapipb.Resources{
				CpuMillicores: 2000,
				MemoryBytes:   4 << 30,
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResourceLimits(tt.reqs)
			if !proto.Equal(got, tt.want) {
				t.Fatalf("ResourceLimits() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildActorTemplate_Resources(t *testing.T) {
	limits := &ateapipb.ResourceRequirements{Limits: &ateapipb.Resources{
		CpuMillicores: 2000,
		MemoryBytes:   4 << 30,
	}}
	tmpl := BuildActorTemplate("default", "task-tmpl-01234567", "ghcr.io/my-org/agent@sha256:abc", nil, nil, "", limits)
	if !proto.Equal(tmpl.GetResources(), limits) {
		t.Fatalf("template resources = %v, want %v", tmpl.GetResources(), limits)
	}

	// Without explicit limits the template carries the default CPU and memory
	// limits required by Substrate.
	wantDefault := &ateapipb.ResourceRequirements{Limits: &ateapipb.Resources{
		CpuMillicores: DefaultCPUMillicores,
		MemoryBytes:   DefaultMemoryBytes,
	}}
	tmpl = BuildActorTemplate("default", "task-tmpl-01234567", "ghcr.io/my-org/agent@sha256:abc", nil, nil, "", nil)
	if !proto.Equal(tmpl.GetResources(), wantDefault) {
		t.Fatalf("template without limits has resources %v, want %v", tmpl.GetResources(), wantDefault)
	}
}

func TestClient_TagsAndEnsureActorFromTag(t *testing.T) {
	oldInterval := defaultRetryInterval
	defaultRetryInterval = 5 * time.Millisecond
	defer func() { defaultRetryInterval = oldInterval }()

	mock := &mockControlClient{
		createTagErrs: []error{
			status.Error(codes.Unavailable, "transient"),
			status.Error(codes.Aborted, "actor lease busy"),
			nil,
		},
		deleteTagErrs: []error{status.Error(codes.Aborted, "busy"), nil},
	}
	c := &Client{control: mock}
	ctx := context.Background()

	tmpl := BuildActorTemplate("default", "ax-demo", "us-docker.pkg.dev/p/img@sha256:000", nil, nil, "", nil)
	ensured, err := c.EnsureActorTemplate(ctx, tmpl)
	if err != nil {
		t.Fatalf("EnsureActorTemplate: %v", err)
	}
	if ensured.GetMetadata().GetName() != "ax-demo" {
		t.Errorf("template name = %q, want ax-demo", ensured.GetMetadata().GetName())
	}

	tag, err := c.CreateTag(ctx, "default", "ws-demo-tag", "setup-actor-1")
	if err != nil {
		t.Fatalf("CreateTag: %v", err)
	}
	if tag.GetScope() != ateapipb.TagScope_TAG_SCOPE_ATESPACE || tag.GetSourceActor().GetName() != "setup-actor-1" {
		t.Fatalf("unexpected created tag: %+v", tag)
	}

	gotTag, err := c.GetTag(ctx, "default", "ws-demo-tag")
	if err != nil || gotTag.GetStatus().GetActorTemplateUid() != "tpl-uid-123" {
		t.Fatalf("GetTag = %+v, %v", gotTag, err)
	}

	tags, err := c.ListTags(ctx, "default")
	if err != nil || len(tags) != 1 || tags[0].GetMetadata().GetName() != "ws-demo-tag" {
		t.Fatalf("ListTags = %+v, %v", tags, err)
	}

	actor, err := c.EnsureActorFromTag(ctx, "default", "task-1", "default", "ax-demo", "ws-demo-tag")
	if err != nil {
		t.Fatalf("EnsureActorFromTag: %v", err)
	}
	if actor.GetSourceTag().GetName() != "ws-demo-tag" {
		t.Errorf("source_tag = %+v, want ws-demo-tag", actor.GetSourceTag())
	}

	if err := c.DeleteTag(ctx, "default", "ws-demo-tag"); err != nil {
		t.Fatalf("DeleteTag: %v", err)
	}
	// Deleting again when already gone is idempotent.
	if err := c.DeleteTag(ctx, "default", "ws-demo-tag"); err != nil {
		t.Fatalf("second DeleteTag (NotFound) should succeed, got %v", err)
	}
}

func TestClient_EnsureActor_DeletingRaces(t *testing.T) {
	oldRetry, oldWait := defaultRetryInterval, actorDeletingWaitInterval
	defaultRetryInterval, actorDeletingWaitInterval = 2*time.Millisecond, 2*time.Millisecond
	defer func() {
		defaultRetryInterval = oldRetry
		actorDeletingWaitInterval = oldWait
	}()

	t.Run("recreates when deleting actor disappears before GetActor", func(t *testing.T) {
		c := &Client{control: &mockControlClient{
			actorRemaining:  0, // GetActor immediately sees NotFound
			createActorErrs: []error{status.Error(codes.AlreadyExists, "actor deleting"), nil},
		}}
		actor, err := c.EnsureActorFromTag(context.Background(), "default", "task-1", "default", "ax-demo", "ws-demo-tag")
		if err != nil {
			t.Fatalf("EnsureActorFromTag failed: %v", err)
		}
		if actor.GetSourceTag().GetName() != "ws-demo-tag" {
			t.Fatalf("unexpected recreated actor: %+v", actor)
		}
	})

	t.Run("propagates non-AlreadyExists error during recreateDeletingActor", func(t *testing.T) {
		c := &Client{control: &mockControlClient{
			actorRemaining: 1, // GetActor sees ACTOR_STATE_DELETING
			createActorErrs: []error{
				status.Error(codes.AlreadyExists, "actor deleting"),
				status.Error(codes.FailedPrecondition, "Tag not found"),
			},
		}}
		_, err := c.EnsureActorFromTag(context.Background(), "default", "task-1", "default", "ax-demo", "missing-tag")
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("status.Code(err) = %v (%v), want FailedPrecondition", status.Code(err), err)
		}
	})
}
