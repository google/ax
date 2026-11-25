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

	"github.com/google/ax/internal/apivalidation"
	"github.com/google/ax/pkg/apis/v1alpha1"
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
	img := &v1alpha1.Profile{Image: "us-docker.pkg.dev/p/img@sha256:abc"}
	tests := []struct {
		name    string
		spec    *v1alpha1.TaskSpec
		wantErr string
	}{
		{name: "nil spec", wantErr: "spec.profile: image or ateTemplate.name is required unless spec.workspace"},
		{name: "no profile or workspace", spec: &v1alpha1.TaskSpec{Commands: []string{"true"}}, wantErr: "spec.profile: image or ateTemplate.name is required unless spec.workspace"},
		{name: "empty profile without workspace", spec: &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{}}, wantErr: "spec.profile: image or ateTemplate.name is required unless spec.workspace"},
		{name: "no profile inherits workspace", spec: &v1alpha1.TaskSpec{Workspace: &v1alpha1.WorkspaceRef{Name: "a"}}},
		{
			name: "profile resources without image",
			spec: &v1alpha1.TaskSpec{
				Profile: &v1alpha1.Profile{Resources: &v1alpha1.ResourcesSpec{Limits: &v1alpha1.ResourceList{Cpu: "2"}}},
			},
			wantErr: "spec.profile: image or ateTemplate.name is required",
		},
		{name: "no workspace", spec: &v1alpha1.TaskSpec{Profile: img}},
		{name: "default path", spec: &v1alpha1.TaskSpec{Profile: img, Workspace: &v1alpha1.WorkspaceRef{Name: "a"}}},
		{name: "explicit path", spec: &v1alpha1.TaskSpec{Profile: img, Workspace: &v1alpha1.WorkspaceRef{Name: "a", Path: "/x"}}},
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
			name: "profile ateTemplate.name",
			spec: &v1alpha1.TaskSpec{
				Profile: &v1alpha1.Profile{AteTemplate: &v1alpha1.ObjectRef{Name: "base-template"}},
			},
		},
		{
			name: "ateTemplate.atespace invalid name",
			spec: &v1alpha1.TaskSpec{
				Profile: &v1alpha1.Profile{AteTemplate: &v1alpha1.ObjectRef{Name: "base-template", Atespace: "Bad_Space"}},
			},
			wantErr: "spec.profile.ateTemplate.atespace",
		},
		{
			name: "ateTemplate.name empty",
			spec: &v1alpha1.TaskSpec{
				Profile: &v1alpha1.Profile{AteTemplate: &v1alpha1.ObjectRef{Atespace: "shared"}},
			},
			wantErr: "spec.profile.ateTemplate.name: must be set",
		},
		{
			name: "profile ateTemplate.name invalid name",
			spec: &v1alpha1.TaskSpec{
				Profile: &v1alpha1.Profile{AteTemplate: &v1alpha1.ObjectRef{Name: "Bad_Name"}},
			},
			wantErr: "spec.profile.ateTemplate.name",
		},
		{
			name: "profile ateTemplate.name combined with image",
			spec: &v1alpha1.TaskSpec{
				Profile: &v1alpha1.Profile{
					AteTemplate: &v1alpha1.ObjectRef{Name: "base-template"},
					Image:       "us-docker.pkg.dev/p/img@sha256:abc",
				},
			},
			wantErr: "cannot be combined with image or resources",
		},
		{
			name: "profile ateTemplate.name combined with resources",
			spec: &v1alpha1.TaskSpec{
				Profile: &v1alpha1.Profile{
					AteTemplate: &v1alpha1.ObjectRef{Name: "base-template"},
					Resources:   &v1alpha1.ResourcesSpec{Limits: &v1alpha1.ResourceList{Cpu: "1"}},
				},
			},
			wantErr: "cannot be combined with image or resources",
		},
		{
			name:    "nameless binding",
			spec:    &v1alpha1.TaskSpec{Profile: img, Workspace: &v1alpha1.WorkspaceRef{Path: "/w"}},
			wantErr: "spec.workspace: name is required",
		},
		{
			name:    "relative path",
			spec:    &v1alpha1.TaskSpec{Profile: img, Workspace: &v1alpha1.WorkspaceRef{Name: "a", Path: "rel"}},
			wantErr: "spec.workspace: path \"rel\" must be absolute",
		},
		{name: "gateway", spec: &v1alpha1.TaskSpec{Profile: img, Gateway: &v1alpha1.GatewayRef{Name: "gw"}}},
		{
			name:    "nameless gateway",
			spec:    &v1alpha1.TaskSpec{Profile: img, Gateway: &v1alpha1.GatewayRef{}},
			wantErr: "spec.gateway.name",
		},
		{
			name: "goal and session_id",
			spec: &v1alpha1.TaskSpec{
				Profile:   img,
				Goal:      &v1alpha1.Goal{Text: "Fix failing unit tests"},
				SessionId: "session-42",
			},
		},
		{
			name: "config",
			spec: &v1alpha1.TaskSpec{Profile: img, Config: mustStruct(t, map[string]any{"suite": "swe", "shard": map[string]any{"index": 7}})},
		},
		{
			name:    "oversized config",
			spec:    &v1alpha1.TaskSpec{Profile: img, Config: mustStruct(t, map[string]any{"blob": strings.Repeat("x", apivalidation.MaxTaskConfigSize)})},
			wantErr: "spec.config:",
		},
		{
			name: "config near the limit",
			spec: &v1alpha1.TaskSpec{Profile: img, Config: mustStruct(t, map[string]any{"blob": strings.Repeat("x", apivalidation.MaxTaskConfigSize-100)})},
		},
		{
			// Each value fits on its own, but together they do not fit in one
			// Substrate environment variable.
			name: "oversized task",
			spec: &v1alpha1.TaskSpec{
				Profile: img,
				Config:  mustStruct(t, map[string]any{"blob": strings.Repeat("x", apivalidation.MaxTaskConfigSize-100)}),
				Env:     []*v1alpha1.EnvVar{{Name: "BIG", Value: strings.Repeat("y", 16<<10)}},
			},
			wantErr: "task: encoded launch configuration",
		},
		{
			name:    "oversized commands",
			spec:    &v1alpha1.TaskSpec{Profile: img, Commands: []string{"sh", "-c", strings.Repeat("z", apivalidation.MaxEnvValueSize)}},
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

func TestValidateTask_ReservedSetupActorPrefix(t *testing.T) {
	if err := apivalidation.ValidateTask(&v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: apivalidation.SetupActorPrefix + "foo"},
	}); err == nil || !strings.Contains(err.Error(), "reserved for workspace setup actors") {
		t.Fatalf("ValidateTask(reserved prefix) = %v, want reserved prefix error", err)
	}
	if err := apivalidation.ValidateTask(&v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "my-" + apivalidation.SetupActorPrefix + "task"},
		Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "us-docker.pkg.dev/p/img@sha256:abc"}},
	}); err != nil {
		t.Fatalf("ValidateTask(non-prefix occurrence) = %v, want nil", err)
	}
}
