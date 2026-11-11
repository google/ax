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
			wantErr: "spec.gateway: name is required",
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
			name:    "oversized command",
			spec:    &v1alpha1.TaskSpec{Command: []string{"sh", "-c", strings.Repeat("z", apivalidation.MaxEnvValueSize)}},
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
