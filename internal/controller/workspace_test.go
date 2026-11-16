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
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/ax/internal/apivalidation"
	"github.com/google/ax/internal/controller"
	"github.com/google/ax/internal/substrate/substratetest"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeSetupCoordinator struct {
	calls        atomic.Int32
	active       atomic.Int32
	registerHook func(ctx context.Context, atespace, setupActor, actorUID string, ws *v1alpha1.Workspace) error
	fn           func(ctx context.Context, atespace, setupActor string) error
}

func (f *fakeSetupCoordinator) RegisterSetup(ctx context.Context, atespace, setupActor, actorUID string, ws *v1alpha1.Workspace) error {
	f.active.Add(1)
	if f.registerHook != nil {
		return f.registerHook(ctx, atespace, setupActor, actorUID, ws)
	}
	return nil
}

func (f *fakeSetupCoordinator) AwaitSetup(ctx context.Context, atespace, setupActor string) error {
	f.calls.Add(1)
	if f.fn != nil {
		return f.fn(ctx, atespace, setupActor)
	}
	return nil
}

func (f *fakeSetupCoordinator) DeleteSetup(context.Context, string, string) error {
	f.active.Add(-1)
	return nil
}

func TestWorkspaceTagName(t *testing.T) {
	tpl1 := "ax-3f9c1a2b5d7e9f01"
	tpl2 := "ax-9999999999999999"

	uid1 := "11111111-1111-1111-1111-111111111111"
	uid2 := "22222222-2222-2222-2222-222222222222"

	short1 := controller.WorkspaceTagName(uid1, tpl1, 1)
	short2 := controller.WorkspaceTagName(uid1, tpl2, 1)
	refreshed := controller.WorkspaceTagName(uid1, tpl1, 12)
	if err := apivalidation.ValidateName(short1); err != nil {
		t.Fatalf("short tag %q invalid: %v", short1, err)
	}
	if !strings.HasSuffix(short1, "-r1") || !strings.HasSuffix(refreshed, "-r12") {
		t.Fatalf("unexpected revision suffixes: %q, %q", short1, refreshed)
	}
	prefix := controller.WorkspaceTagPrefix(uid1)
	if !strings.HasPrefix(short1, prefix) || !strings.HasPrefix(short2, prefix) || !strings.HasPrefix(refreshed, prefix) {
		t.Errorf("tags %q, %q, %q do not share WorkspaceTagPrefix %q", short1, short2, refreshed, prefix)
	}

	tag1 := controller.WorkspaceTagName(uid1, tpl1, 1)
	tag2 := controller.WorkspaceTagName(uid2, tpl1, math.MaxUint64)
	if err := apivalidation.ValidateName(tag1); err != nil {
		t.Fatalf("tag1 %q invalid: %v", tag1, err)
	}
	if err := apivalidation.ValidateName(tag2); err != nil {
		t.Fatalf("tag2 %q invalid at MaxUint64: %v", tag2, err)
	}
	if controller.WorkspaceTagPrefix(uid1) == controller.WorkspaceTagPrefix(uid2) {
		t.Errorf("distinct workspace UIDs collided on owner prefix: %q vs %q", tag1, tag2)
	}
}

func TestNeedsPrewarmSnapshot(t *testing.T) {
	tests := []struct {
		name string
		spec *v1alpha1.WorkspaceSpec
		want bool
	}{
		{name: "nil_spec", spec: nil, want: false},
		{name: "empty_spec", spec: &v1alpha1.WorkspaceSpec{}, want: false},
		{name: "empty_profile", spec: &v1alpha1.WorkspaceSpec{Profile: &v1alpha1.Profile{}}, want: false},
		{
			name: "mcp_only",
			spec: &v1alpha1.WorkspaceSpec{
				Mcp: &v1alpha1.MCPConfig{Servers: []*v1alpha1.MCPServer{{Name: "s", Endpoint: "http://mcp"}}},
			},
			want: false,
		},
		{
			name: "setup_git",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{Git: []*v1alpha1.GitRepo{{Repo: "https://github.com/org/repo.git"}}},
			},
			want: true,
		},
		{
			name: "setup_files",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{Files: []*v1alpha1.File{{Path: "AGENTS.md", Content: "hi"}}},
			},
			want: true,
		},
		{
			name: "setup_skills",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{Skills: &v1alpha1.SkillsConfig{Path: ".ax/skills"}},
			},
			want: true,
		},
		{
			name: "setup_commands",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{Commands: []string{"npm ci"}},
			},
			want: true,
		},
		{
			name: "profile_image",
			spec: &v1alpha1.WorkspaceSpec{
				Profile: &v1alpha1.Profile{Image: "img@sha256:abc"},
			},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sub := substratetest.NewServer(t)
			coord := &fakeSetupCoordinator{}
			mgr := controller.NewWorkspaceManager(sub.Client(), controller.WorkspaceManagerOptions{
				SetupCoordinator: coord,
			})
			st, err := mgr.Prepare(t.Context(), &v1alpha1.Workspace{
				Metadata: &v1alpha1.ObjectMeta{Name: "ws", Atespace: "default", Uid: "uid-ws"},
				Spec:     tc.spec,
			}, nil)
			if err != nil || st.GetPhase() != v1alpha1.WorkspacePhaseReady {
				t.Fatalf("Prepare(%+v) = (%+v, %v), want Ready", tc.spec, st, err)
			}
			if got := coord.calls.Load() > 0; got != tc.want {
				t.Errorf("Prepare(%+v) ran setup actor = %v, want %v", tc.spec, got, tc.want)
			}
		})
	}
}

func TestWorkspaceManager_PrepareCoalesceAndDelete(t *testing.T) {
	ctx := t.Context()
	sub := substratetest.NewServer(t)

	var (
		registeredBeforeResume atomic.Bool
		sawGatewayPolicy       atomic.Bool
	)
	coord := &fakeSetupCoordinator{
		registerHook: func(ctx context.Context, atespace, setupActor, actorUID string, _ *v1alpha1.Workspace) error {
			if err := apivalidation.ValidateName(setupActor); err != nil {
				t.Errorf("invalid setup actor name %q: %v", setupActor, err)
			}
			if actorUID == "" {
				t.Errorf("RegisterSetup(%q) received empty actorUID", setupActor)
			}
			actor, err := sub.Client().GetActor(ctx, atespace, setupActor)
			if err == nil && actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
				registeredBeforeResume.Store(true)
			}
			return nil
		},
		fn: func(_ context.Context, atespace, setupActor string) error {
			if _, ok := sub.EgressPolicy(atespace, setupActor); ok {
				sawGatewayPolicy.Store(true)
			}
			return nil
		},
	}
	mgr := controller.NewWorkspaceManager(sub.Client(), controller.WorkspaceManagerOptions{
		SetupCoordinator: coord,
		DefaultTaskImage: "ghcr.io/org/default-task@sha256:111",
		RunnerImage:      "ghcr.io/org/runner@sha256:222",
	})

	// 1. MCP-only workspace jumps straight to Ready without running a setup actor.
	st, err := mgr.Prepare(ctx, &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "mcp-only", Atespace: "default"},
		Spec: &v1alpha1.WorkspaceSpec{
			Mcp: &v1alpha1.MCPConfig{Servers: []*v1alpha1.MCPServer{{Name: "git", Endpoint: "http://mcp"}}},
		},
	}, nil)
	if err != nil || st.GetPhase() != v1alpha1.WorkspacePhaseReady {
		t.Fatalf("Prepare(mcp-only) = (%+v, %v), want Ready", st, err)
	}
	if coord.calls.Load() != 0 {
		t.Fatalf("expected 0 setup actor runs for mcp-only workspace, got %d", coord.calls.Load())
	}

	// 2. Workspace with setup + setupGateway runs setup actor, binds egress rules,
	// creates default Tag, and deletes temporary setup actor.
	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "payments", Atespace: "default", Uid: "uid-payments"},
		Spec: &v1alpha1.WorkspaceSpec{
			Setup: &v1alpha1.WorkspaceSetup{
				Commands: []string{"npm ci"},
				Gateway:  &v1alpha1.GatewayRef{Name: "build-gw"},
			},
		},
	}
	gw := &v1alpha1.Gateway{
		Metadata: &v1alpha1.ObjectMeta{Name: "build-gw", Atespace: "default"},
		Spec: &v1alpha1.GatewaySpec{
			Egress: []*ateapipb.EgressRule{{
				Https: &ateapipb.HttpsRule{Hostnames: []string{"registry.npmjs.org"}},
			}},
		},
	}
	st, err = mgr.Prepare(ctx, ws, gw)
	if err != nil || st.GetPhase() != v1alpha1.WorkspacePhaseReady {
		t.Fatalf("Prepare(payments) = (%+v, %v), want Ready", st, err)
	}
	if !registeredBeforeResume.Load() {
		t.Error("RegisterSetup was not called before ResumeActor")
	}
	if !sawGatewayPolicy.Load() {
		t.Error("setup actor did not have setupGateway egress policy bound during AwaitSetup")
	}
	if got := coord.active.Load(); got != 0 {
		t.Errorf("active setup records after Prepare = %d, want 0 (DeleteSetup not called)", got)
	}
	conds := st.GetConditions()
	if len(conds) != 2 ||
		conds[0].GetType() != v1alpha1.WorkspaceConditionSetupComplete || conds[0].GetStatus() != "True" ||
		conds[1].GetType() != v1alpha1.WorkspaceConditionSnapshotReady || conds[1].GetStatus() != "True" {
		t.Fatalf("conditions = %+v, want SetupComplete=True and SnapshotReady=True", conds)
	}
	if coord.calls.Load() != 1 {
		t.Fatalf("AwaitSetup calls = %d, want 1", coord.calls.Load())
	}
	actors, err := sub.Client().ListActors(ctx, "default")
	if err != nil || len(actors) != 0 {
		t.Fatalf("expected temporary setup actor to be cleaned up, found actors=%+v err=%v", actors, err)
	}

	// Warm hit on default profile does not run another setup actor.
	defaultTag, err := mgr.EnsureSnapshotTag(ctx, ws, nil, gw)
	if err != nil || defaultTag == "" {
		t.Fatalf("EnsureSnapshotTag(warm hit) = (%q, %v)", defaultTag, err)
	}
	if coord.calls.Load() != 1 {
		t.Fatalf("warm hit ran AwaitSetup again: calls = %d", coord.calls.Load())
	}

	// If an incomplete tag (unset Snapshot or PENDING state) was stranded by an
	// interrupted CreateTag, EnsureSnapshotTag deletes and rebuilds it instead of
	// treating it as ready.
	sub.SetTag(&ateapipb.Tag{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "default", Name: defaultTag},
		Status:   &ateapipb.TagStatus{},
	})
	if _, err := mgr.EnsureSnapshotTag(ctx, ws, nil, gw); err != nil {
		t.Fatalf("EnsureSnapshotTag(rebuild unset snapshot tag): %v", err)
	}
	if coord.calls.Load() != 2 {
		t.Fatalf("expected unset-snapshot tag to trigger rebuild, calls = %d, want 2", coord.calls.Load())
	}

	sub.SetTag(&ateapipb.Tag{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "default", Name: defaultTag},
		Status: &ateapipb.TagStatus{
			Snapshot: &ateapipb.Snapshot{
				DurableSnapshot: &ateapipb.SnapshotStorage{State: ateapipb.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_PENDING},
			},
		},
	})
	if _, err := mgr.EnsureSnapshotTag(ctx, ws, nil, gw); err != nil {
		t.Fatalf("EnsureSnapshotTag(rebuild pending tag): %v", err)
	}
	if coord.calls.Load() != 3 {
		t.Fatalf("expected PENDING tag to trigger rebuild, calls = %d, want 3", coord.calls.Load())
	}

	// 3. Concurrent cold misses for a lazy-cached override profile coalesce into 1 setup actor run.
	overrideProf := &v1alpha1.Profile{Image: "ghcr.io/org/custom@sha256:999"}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, callErr := mgr.EnsureSnapshotTag(ctx, ws, overrideProf, gw)
			errs <- callErr
		}()
	}
	wg.Wait()
	close(errs)
	for callErr := range errs {
		if callErr != nil {
			t.Fatalf("concurrent EnsureSnapshotTag failed: %v", callErr)
		}
	}
	if coord.calls.Load() != 4 {
		t.Fatalf("AwaitSetup calls after 8 concurrent cold misses = %d, want 4", coord.calls.Load())
	}

	// Prepare an unrelated workspace to ensure Delete("uid-payments") leaves its tag intact.
	otherWS := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "payments-other", Atespace: "default", Uid: "uid-payments-other"},
		Spec:     &v1alpha1.WorkspaceSpec{Setup: &v1alpha1.WorkspaceSetup{Commands: []string{"true"}}},
	}
	if _, err := mgr.Prepare(ctx, otherWS, nil); err != nil {
		t.Fatalf("Prepare(payments-other): %v", err)
	}

	// Verify DefaultTaskImage was used for the template created when profile.image is omitted.
	expectedTplName, _, err := controller.ResolveProfileTemplate("default", nil, "ghcr.io/org/default-task@sha256:111", "ghcr.io/org/runner@sha256:222")
	if err != nil {
		t.Fatalf("ResolveProfileTemplate: %v", err)
	}
	tpl, err := sub.Client().GetActorTemplate(ctx, "default", expectedTplName)
	if err != nil {
		t.Fatalf("GetActorTemplate(%s): %v", expectedTplName, err)
	}
	if got := tpl.GetContainers()[0].GetImage(); got != "ghcr.io/org/default-task@sha256:111" {
		t.Errorf("template image = %q, want DefaultTaskImage", got)
	}

	// 4. Delete("uid-payments") removes both the eager default tag and the lazy override tag,
	// while keeping "uid-payments-other"'s tag intact.
	if err := mgr.Delete(ctx, "default", "uid-payments"); err != nil {
		t.Fatalf("Delete(uid-payments): %v", err)
	}
	remainingTags, err := sub.Client().ListTags(ctx, "default")
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(remainingTags) != 1 || !strings.HasPrefix(remainingTags[0].GetMetadata().GetName(), controller.WorkspaceTagPrefix("uid-payments-other")) {
		t.Fatalf("remaining tags after Delete(uid-payments) = %+v, want only uid-payments-other's tag", remainingTags)
	}
}

func TestWorkspaceManager_RetriesAndFailures(t *testing.T) {
	t.Run("flaky_setup_retries_with_fresh_actor", func(t *testing.T) {
		ctx := t.Context()
		sub := substratetest.NewServer(t)
		var (
			mu         sync.Mutex
			seenActors []string
			attempt    atomic.Int32
		)
		coord := &fakeSetupCoordinator{
			fn: func(_ context.Context, _, setupActor string) error {
				mu.Lock()
				seenActors = append(seenActors, setupActor)
				mu.Unlock()
				if attempt.Add(1) == 1 {
					return errors.New("temporary git remote 502")
				}
				return nil
			},
		}
		mgr := controller.NewWorkspaceManager(sub.Client(), controller.WorkspaceManagerOptions{
			SetupCoordinator: coord,
			MaxSetupAttempts: 2,
		})
		st, err := mgr.Prepare(ctx, &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: "flaky-ws", Atespace: "default", Uid: "uid-flaky"},
			Spec:     &v1alpha1.WorkspaceSpec{Setup: &v1alpha1.WorkspaceSetup{Commands: []string{"git fetch"}}},
		}, nil)
		if err != nil || st.GetPhase() != v1alpha1.WorkspacePhaseReady || coord.calls.Load() != 2 {
			t.Fatalf("Prepare(flaky) = (%+v, %v), calls=%d, want Ready after 2 attempts", st, err, coord.calls.Load())
		}
		if len(seenActors) != 2 || seenActors[0] == seenActors[1] {
			t.Errorf("setup actor names across attempts = %v, want 2 distinct actor names", seenActors)
		}
		if !strings.HasPrefix(seenActors[0], controller.SetupActorPrefix) {
			t.Errorf("setup actor name %q missing %s prefix", seenActors[0], controller.SetupActorPrefix)
		}
		if got := coord.active.Load(); got != 0 {
			t.Errorf("active setup records after retries = %d, want 0", got)
		}
	})

	t.Run("setup_failure_marks_failed_and_cleans_up_actor", func(t *testing.T) {
		ctx := t.Context()
		sub := substratetest.NewServer(t)
		coord := &fakeSetupCoordinator{
			fn: func(context.Context, string, string) error {
				return errors.New("npm ERR! code E404")
			},
		}
		mgr := controller.NewWorkspaceManager(sub.Client(), controller.WorkspaceManagerOptions{
			SetupCoordinator: coord,
			MaxSetupAttempts: 2,
		})
		ws := &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: "broken-ws", Atespace: "default", Uid: "uid-broken"},
			Spec:     &v1alpha1.WorkspaceSpec{Setup: &v1alpha1.WorkspaceSetup{Commands: []string{"npm ci"}}},
		}
		st, err := mgr.Prepare(ctx, ws, nil)
		if err == nil {
			t.Fatal("expected error from Prepare on setup failure")
		}
		if st.GetPhase() != v1alpha1.WorkspacePhaseFailed || !strings.Contains(st.GetLastSetupError(), "E404") {
			t.Fatalf("status = %+v, want Failed with E404 in LastSetupError", st)
		}
		if actors, _ := sub.Client().ListActors(ctx, "default"); len(actors) != 0 {
			t.Errorf("setup actor leaked after failure: %+v", actors)
		}
		if tags, _ := sub.Client().ListTags(ctx, "default"); len(tags) != 0 {
			t.Errorf("unexpected tag created after failure: %+v", tags)
		}
	})

	t.Run("gateway_rejection_fails_fast", func(t *testing.T) {
		ctx := t.Context()
		sub := substratetest.NewServer(t)
		sub.SetErrors(substratetest.Errors{
			CreateEgressPolicy: status.Error(codes.InvalidArgument, "invalid hostname wildcard"),
		})
		coord := &fakeSetupCoordinator{}
		mgr := controller.NewWorkspaceManager(sub.Client(), controller.WorkspaceManagerOptions{
			SetupCoordinator: coord,
			MaxSetupAttempts: 3,
		})
		ws := &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: "bad-gw-ws", Atespace: "default", Uid: "uid-bad-gw"},
			Spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Commands: []string{"true"},
					Gateway:  &v1alpha1.GatewayRef{Name: "bad-gw"},
				},
			},
		}
		gw := &v1alpha1.Gateway{Metadata: &v1alpha1.ObjectMeta{Name: "bad-gw", Atespace: "default"}}
		st, err := mgr.Prepare(ctx, ws, gw)
		if err == nil || st.GetPhase() != v1alpha1.WorkspacePhaseFailed {
			t.Fatalf("Prepare(bad gateway) = (%+v, %v), want Failed", st, err)
		}
		if sub.DeleteActorCalls() != 1 {
			t.Errorf("expected fail-fast (1 setup actor created & deleted), got DeleteActorCalls=%d", sub.DeleteActorCalls())
		}
		conds := st.GetConditions()
		if len(conds) == 0 || conds[0].GetReason() != v1alpha1.ReasonGatewayBindingFailed {
			t.Fatalf("conditions = %+v, want non-empty with reason %q", conds, v1alpha1.ReasonGatewayBindingFailed)
		}
	})

	t.Run("missing_actor_template_reports_template_creation_failed", func(t *testing.T) {
		ctx := t.Context()
		sub := substratetest.NewServer(t)
		mgr := controller.NewWorkspaceManager(sub.Client(), controller.WorkspaceManagerOptions{
			SetupCoordinator: &fakeSetupCoordinator{},
		})
		ws := &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: "missing-tpl-ws", Atespace: "default", Uid: "uid-missing-tpl"},
			Spec: &v1alpha1.WorkspaceSpec{
				Profile: &v1alpha1.Profile{AteActorTemplate: "does-not-exist"},
			},
		}
		st, err := mgr.Prepare(ctx, ws, nil)
		if err == nil || st.GetPhase() != v1alpha1.WorkspacePhaseFailed {
			t.Fatalf("Prepare(missing template) = (%+v, %v), want Failed", st, err)
		}
		conds := st.GetConditions()
		if len(conds) == 0 || conds[0].GetReason() != v1alpha1.ReasonTemplateCreationFailed {
			t.Fatalf("conditions = %+v, want non-empty with reason %q", conds, v1alpha1.ReasonTemplateCreationFailed)
		}
	})

	t.Run("snapshot_tag_failure_records_setup_complete_true", func(t *testing.T) {
		ctx := t.Context()
		sub := substratetest.NewServer(t)
		sub.SetErrors(substratetest.Errors{
			CreateTag: status.Error(codes.InvalidArgument, "tag quota exceeded"),
		})
		mgr := controller.NewWorkspaceManager(sub.Client(), controller.WorkspaceManagerOptions{
			SetupCoordinator: &fakeSetupCoordinator{},
			MaxSetupAttempts: 1,
		})
		ws := &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: "tag-fail-ws", Atespace: "default", Uid: "uid-tag-fail"},
			Spec:     &v1alpha1.WorkspaceSpec{Setup: &v1alpha1.WorkspaceSetup{Commands: []string{"true"}}},
		}
		st, err := mgr.Prepare(ctx, ws, nil)
		if err == nil || st.GetPhase() != v1alpha1.WorkspacePhaseFailed {
			t.Fatalf("Prepare(tag fail) = (%+v, %v), want Failed", st, err)
		}
		conds := st.GetConditions()
		if len(conds) != 2 ||
			conds[0].GetType() != v1alpha1.WorkspaceConditionSetupComplete || conds[0].GetStatus() != "True" ||
			conds[1].GetType() != v1alpha1.WorkspaceConditionSnapshotReady || conds[1].GetStatus() != "False" ||
			conds[1].GetReason() != v1alpha1.WorkspaceReasonSnapshotTagFailed {
			t.Fatalf("conditions = %+v, want SetupComplete=True and SnapshotReady=False(SnapshotTagFailed)", conds)
		}
	})

	t.Run("transient_substrate_error_stays_preparing", func(t *testing.T) {
		ctx := t.Context()
		sub := substratetest.NewServer(t)
		sub.SetErrors(substratetest.Errors{
			CreateAtespace: status.Error(codes.DeadlineExceeded, "control plane timeout"),
		})
		mgr := controller.NewWorkspaceManager(sub.Client(), controller.WorkspaceManagerOptions{
			SetupCoordinator: &fakeSetupCoordinator{},
		})
		ws := &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: "transient-ws", Atespace: "default", Uid: "uid-transient"},
			Spec:     &v1alpha1.WorkspaceSpec{Setup: &v1alpha1.WorkspaceSetup{Commands: []string{"true"}}},
		}
		st, err := mgr.Prepare(ctx, ws, nil)
		if err == nil || st.GetPhase() != v1alpha1.WorkspacePhasePreparing {
			t.Fatalf("Prepare(transient) = (%+v, %v), want Preparing", st, err)
		}
	})

	t.Run("grpc_canceled_substrate_error_stays_preparing", func(t *testing.T) {
		ctx := t.Context()
		sub := substratetest.NewServer(t)
		sub.SetErrors(substratetest.Errors{
			CreateTag: status.Error(codes.Canceled, "context canceled"),
		})
		coord := &fakeSetupCoordinator{}
		mgr := controller.NewWorkspaceManager(sub.Client(), controller.WorkspaceManagerOptions{
			SetupCoordinator: coord,
			MaxSetupAttempts: 2,
		})
		ws := &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: "grpc-canceled-ws", Atespace: "default", Uid: "uid-grpc-canceled"},
			Spec:     &v1alpha1.WorkspaceSpec{Setup: &v1alpha1.WorkspaceSetup{Commands: []string{"true"}}},
		}
		st, err := mgr.Prepare(ctx, ws, nil)
		if err == nil || status.Code(err) != codes.Canceled {
			t.Fatalf("Prepare(grpc canceled) err = %v, want Canceled", err)
		}
		if st.GetPhase() != v1alpha1.WorkspacePhasePreparing {
			t.Fatalf("Prepare(grpc canceled) phase = %q, want Preparing", st.GetPhase())
		}
		if coord.calls.Load() != 1 {
			t.Errorf("AwaitSetup calls = %d, want 1 (should not retry after Canceled)", coord.calls.Load())
		}
	})
}

func TestWorkspaceManager_ParallelProfilesUnderSharedLock(t *testing.T) {
	ctx := t.Context()
	sub := substratetest.NewServer(t)

	// Barrier inside AwaitSetup: both distinct profiles must enter AwaitSetup
	// concurrently. If EnsureSnapshotTag held an exclusive Workspace lock rather
	// than RLock, the second profile would never reach the barrier.
	var arrived atomic.Int32
	bothInSetup := make(chan struct{})
	coord := &fakeSetupCoordinator{
		fn: func(ctx context.Context, _, _ string) error {
			if arrived.Add(1) == 2 {
				close(bothInSetup)
			}
			select {
			case <-bothInSetup:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}

	mgr := controller.NewWorkspaceManager(sub.Client(), controller.WorkspaceManagerOptions{
		SetupCoordinator: coord,
	})
	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "parallel-ws", Atespace: "default", Uid: "uid-parallel"},
		Spec:     &v1alpha1.WorkspaceSpec{Setup: &v1alpha1.WorkspaceSetup{Commands: []string{"true"}}},
		Status:   &v1alpha1.WorkspaceStatus{Phase: v1alpha1.WorkspacePhaseReady},
	}

	profA := &v1alpha1.Profile{Image: "ghcr.io/org/a@sha256:aaa"}
	profB := &v1alpha1.Profile{Image: "ghcr.io/org/b@sha256:bbb"}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, prof := range []*v1alpha1.Profile{profA, profB} {
		wg.Add(1)
		go func(p *v1alpha1.Profile) {
			defer wg.Done()
			_, err := mgr.EnsureSnapshotTag(ctx, ws, p, nil)
			errs <- err
		}(prof)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("parallel EnsureSnapshotTag failed: %v", err)
		}
	}
}
