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

package apivalidation_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/ax/internal/apivalidation"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
)

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestValidateTask(t *testing.T) {
	tests := []struct {
		name    string
		spec    *v1alpha1.TaskSpec
		wantErr string
	}{
		{name: "nil spec"},
		{name: "no workspace", spec: &v1alpha1.TaskSpec{}},
		{name: "default path", spec: &v1alpha1.TaskSpec{Workspace: &v1alpha1.WorkspaceRef{Name: "a"}}},
		{name: "explicit path", spec: &v1alpha1.TaskSpec{Workspace: &v1alpha1.WorkspaceRef{Name: "a", Path: "/x"}}},
		{
			name: "profile image and resources",
			spec: &v1alpha1.TaskSpec{
				Profile: &v1alpha1.Profile{
					Image: "us-docker.pkg.dev/p/img@sha256:abc",
					Resources: &v1alpha1.ResourcesSpec{
						Limits: &v1alpha1.ResourceList{Cpu: "2", Memory: "4Gi"},
					},
				},
			},
		},
		{
			name: "profile ate_actor_template",
			spec: &v1alpha1.TaskSpec{
				Profile: &v1alpha1.Profile{AteActorTemplate: "base-template"},
			},
		},
		{
			name: "profile ate_actor_template invalid name",
			spec: &v1alpha1.TaskSpec{
				Profile: &v1alpha1.Profile{AteActorTemplate: "Bad_Name"},
			},
			wantErr: "spec.profile.ateActorTemplate",
		},
		{
			name: "profile ate_actor_template combined with image",
			spec: &v1alpha1.TaskSpec{
				Profile: &v1alpha1.Profile{
					AteActorTemplate: "base-template",
					Image:            "us-docker.pkg.dev/p/img@sha256:abc",
				},
			},
			wantErr: "cannot be combined with image or resources",
		},
		{
			name: "profile ate_actor_template combined with resources",
			spec: &v1alpha1.TaskSpec{
				Profile: &v1alpha1.Profile{
					AteActorTemplate: "base-template",
					Resources:        &v1alpha1.ResourcesSpec{Limits: &v1alpha1.ResourceList{Cpu: "1"}},
				},
			},
			wantErr: "cannot be combined with image or resources",
		},
		{
			name:    "nameless binding",
			spec:    &v1alpha1.TaskSpec{Workspace: &v1alpha1.WorkspaceRef{Path: "/w"}},
			wantErr: "spec.workspace: name is required",
		},
		{
			name:    "relative path",
			spec:    &v1alpha1.TaskSpec{Workspace: &v1alpha1.WorkspaceRef{Name: "a", Path: "rel"}},
			wantErr: "spec.workspace: path \"rel\" must be absolute",
		},
		{name: "gateway", spec: &v1alpha1.TaskSpec{Gateway: &v1alpha1.GatewayRef{Name: "gw"}}},
		{
			name:    "nameless gateway",
			spec:    &v1alpha1.TaskSpec{Gateway: &v1alpha1.GatewayRef{}},
			wantErr: "spec.gateway.name",
		},
		{
			name: "goal retry and session_id",
			spec: &v1alpha1.TaskSpec{
				Goal:      &v1alpha1.Goal{Spec: "Fix failing unit tests"},
				SessionId: "session-42",
				Retry: &v1alpha1.RetryConfig{
					MaxRetries:        3,
					InitialBackoff:    durationpb.New(10 * time.Second),
					MaxBackoff:        durationpb.New(5 * time.Minute),
					BackoffMultiplier: 2.0,
				},
			},
		},
		{
			name:    "negative max_retries",
			spec:    &v1alpha1.TaskSpec{Retry: &v1alpha1.RetryConfig{MaxRetries: -1}},
			wantErr: "spec.retry.maxRetries",
		},
		{
			name:    "negative initial_backoff",
			spec:    &v1alpha1.TaskSpec{Retry: &v1alpha1.RetryConfig{InitialBackoff: durationpb.New(-time.Second)}},
			wantErr: "spec.retry.initialBackoff",
		},
		{
			name:    "negative max_backoff",
			spec:    &v1alpha1.TaskSpec{Retry: &v1alpha1.RetryConfig{MaxBackoff: durationpb.New(-time.Second)}},
			wantErr: "spec.retry.maxBackoff",
		},
		{
			name: "initial_backoff exceeds max_backoff",
			spec: &v1alpha1.TaskSpec{Retry: &v1alpha1.RetryConfig{
				InitialBackoff: durationpb.New(2 * time.Minute),
				MaxBackoff:     durationpb.New(time.Minute),
			}},
			wantErr: "spec.retry.initialBackoff: must not exceed maxBackoff",
		},
		{
			name:    "backoff_multiplier below 1",
			spec:    &v1alpha1.TaskSpec{Retry: &v1alpha1.RetryConfig{BackoffMultiplier: 0.5}},
			wantErr: "spec.retry.backoffMultiplier",
		},
		{
			name: "config",
			spec: &v1alpha1.TaskSpec{Config: mustStruct(t, map[string]any{"suite": "swe", "shard": map[string]any{"index": 7}})},
		},
		{
			name:    "oversized config",
			spec:    &v1alpha1.TaskSpec{Config: mustStruct(t, map[string]any{"blob": strings.Repeat("x", apivalidation.MaxTaskConfigSize)})},
			wantErr: "spec.config:",
		},
		{
			name: "config near the limit",
			spec: &v1alpha1.TaskSpec{Config: mustStruct(t, map[string]any{"blob": strings.Repeat("x", apivalidation.MaxTaskConfigSize-100)})},
		},
		{
			// Each value fits on its own, but together they do not fit in one
			// Substrate environment variable.
			name: "oversized task",
			spec: &v1alpha1.TaskSpec{
				Config: mustStruct(t, map[string]any{"blob": strings.Repeat("x", apivalidation.MaxTaskConfigSize-100)}),
				Env:    []*v1alpha1.EnvVar{{Name: "BIG", Value: strings.Repeat("y", 16<<10)}},
			},
			wantErr: "task: encoded launch configuration",
		},
		{
			name:    "oversized commands",
			spec:    &v1alpha1.TaskSpec{Commands: []string{"sh", "-c", strings.Repeat("z", apivalidation.MaxEnvValueSize)}},
			wantErr: "task: encoded launch configuration",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := apivalidation.ValidateTask(&v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: "task"}, Spec: tt.spec})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
