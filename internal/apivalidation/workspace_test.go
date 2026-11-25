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
	"context"
	"strings"
	"testing"

	"github.com/google/ax/internal/apivalidation"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// validateWorkspace validates ws as the API server does, in a CreateWorkspace request.
func validateWorkspace(ws *v1alpha1.Workspace) error {
	return apivalidation.ValidateCreateWorkspaceRequest(context.Background(), &v1alpha1.CreateWorkspaceRequest{Workspace: ws}).ToAggregate()
}

func TestValidateCreateWorkspaceRequest(t *testing.T) {
	tests := []struct {
		name string
		req  *v1alpha1.CreateWorkspaceRequest
		want field.ErrorList
	}{
		{name: "valid", req: &v1alpha1.CreateWorkspaceRequest{Workspace: &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "ws"}}}},
		{name: "missing workspace", req: &v1alpha1.CreateWorkspaceRequest{}, want: field.ErrorList{field.Required(field.NewPath("workspace"), "")}},
		{name: "missing workspace.metadata", req: &v1alpha1.CreateWorkspaceRequest{Workspace: &v1alpha1.Workspace{}}, want: field.ErrorList{field.Required(field.NewPath("workspace", "metadata"), "")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidateErr(t, apivalidation.ValidateCreateWorkspaceRequest(context.Background(), tt.req), tt.want)
		})
	}
}

func TestValidateWorkspace_Valid(t *testing.T) {
	tests := []struct {
		name string
		spec *v1alpha1.WorkspaceSpec
	}{
		{name: "nil_spec"},
		{
			name: "full_workspace",
			spec: &v1alpha1.WorkspaceSpec{
				Profile: &v1alpha1.Profile{
					Image: "us-docker.pkg.dev/p/img@sha256:abc",
					Resources: &v1alpha1.ResourcesSpec{
						Limits: &v1alpha1.ResourceList{Cpu: "2", Memory: "4Gi"},
					},
				},
				Setup: &v1alpha1.WorkspaceSetup{
					Git:      []*v1alpha1.GitRepo{{Repo: "https://github.com/org/repo.git", Dir: ".", Ref: "main", Depth: 1}},
					Files:    []*v1alpha1.File{{Path: "AGENTS.md", Content: "hi"}},
					Commands: []string{"go mod download"},
					Gateway:  &v1alpha1.GatewayRef{Name: "build-gw"},
				},
				Mcp: &v1alpha1.MCPConfig{
					Servers: []*v1alpha1.MCPServer{{Name: "gh", Endpoint: "https://example.com/mcp"}},
				},
			},
		},
		{
			name: "local_repo_fixture_path",
			spec: &v1alpha1.WorkspaceSpec{
				Profile: &v1alpha1.Profile{Image: "us-docker.pkg.dev/p/img@sha256:abc"},
				Setup: &v1alpha1.WorkspaceSetup{
					Git: []*v1alpha1.GitRepo{{Repo: "/opt/e2e-fixtures/sample-repo.git", Ref: "main"}},
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateWorkspace(&v1alpha1.Workspace{
				Metadata: &v1alpha1.ObjectMeta{Name: "valid-ws"},
				Spec:     tc.spec,
			})
			if err != nil {
				t.Fatalf("ValidateWorkspace() unexpected error: %v", err)
			}
		})
	}
}

func TestValidateWorkspace_Errors(t *testing.T) {
	tests := []struct {
		name    string
		spec    *v1alpha1.WorkspaceSpec
		wantErr string
	}{
		{
			name: "missing_file_path",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Files: []*v1alpha1.File{{Path: "", Content: "missing path"}},
				},
			},
			wantErr: "spec.setup.files[0]: path is required",
		},
		{
			name: "file_path_escapes_workspace",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Files: []*v1alpha1.File{{Path: "../etc/passwd", Content: "bad"}},
				},
			},
			wantErr: "spec.setup.files[0].path",
		},
		{
			name: "file_path_absolute",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Files: []*v1alpha1.File{{Path: "/etc/passwd", Content: "bad"}},
				},
			},
			wantErr: "spec.setup.files[0].path",
		},
		{
			name: "file_path_dot",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Files: []*v1alpha1.File{{Path: ".", Content: "x"}},
				},
			},
			wantErr: "spec.setup.files[0].path",
		},
		{
			name: "file_path_trailing_slash",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Files: []*v1alpha1.File{{Path: "dir/", Content: "x"}},
				},
			},
			wantErr: "spec.setup.files[0].path",
		},
		{
			name: "missing_git_repo",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Git: []*v1alpha1.GitRepo{{Ref: "main"}},
				},
			},
			wantErr: "spec.setup.git[0]: repo is required",
		},
		{
			name: "ssh_git_repo",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Git: []*v1alpha1.GitRepo{{Repo: "git@github.com:org/repo.git"}},
				},
			},
			wantErr: "SSH git URLs",
		},
		{
			name: "uppercase_ssh_git_repo",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Git: []*v1alpha1.GitRepo{{Repo: "SSH://git@github.com/org/repo.git"}},
				},
			},
			wantErr: "SSH git URLs",
		},
		{
			name: "git_repo_leading_dash",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Git: []*v1alpha1.GitRepo{{Repo: "--upload-pack=bad"}},
				},
			},
			wantErr: "spec.setup.git[0].repo",
		},
		{
			name: "git_ref_option_injection",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Git: []*v1alpha1.GitRepo{{Repo: "https://example.com/r.git", Ref: "--upload-pack=touch /tmp/pwn"}},
				},
			},
			wantErr: "spec.setup.git[0].ref",
		},
		{
			name: "git_ref_whitespace",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Git: []*v1alpha1.GitRepo{{Repo: "https://example.com/r.git", Ref: "main "}},
				},
			},
			wantErr: "spec.setup.git[0].ref",
		},
		{
			name: "negative_git_depth",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Git: []*v1alpha1.GitRepo{{Repo: "https://example.com/r.git", Depth: -1}},
				},
			},
			wantErr: "spec.setup.git[0].depth",
		},
		{
			name: "git_name_escapes_workspace",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Git: []*v1alpha1.GitRepo{{Name: "../escaped-repo", Repo: "https://example.com/r.git"}},
				},
			},
			wantErr: "spec.setup.git[0].name",
		},
		{
			name: "git_name_absolute",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Git: []*v1alpha1.GitRepo{{Name: "/etc/repo", Repo: "https://example.com/r.git"}},
				},
			},
			wantErr: "spec.setup.git[0].name",
		},
		{
			name: "git_dir_escapes_workspace",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Git: []*v1alpha1.GitRepo{{Repo: "https://example.com/r.git", Dir: "../escape"}},
				},
			},
			wantErr: "spec.setup.git[0].dir",
		},
		{
			name: "git_dir_absolute",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Git: []*v1alpha1.GitRepo{{Repo: "https://example.com/r.git", Dir: "/etc"}},
				},
			},
			wantErr: "spec.setup.git[0].dir",
		},
		{
			name: "skills_path_escapes_workspace",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Skills: &v1alpha1.SkillsConfig{Path: "../outside-skills"},
				},
			},
			wantErr: "spec.setup.skills.path",
		},
		{
			name: "invalid_profile_resources",
			spec: &v1alpha1.WorkspaceSpec{
				Profile: &v1alpha1.Profile{
					Resources: &v1alpha1.ResourcesSpec{Limits: &v1alpha1.ResourceList{Cpu: "bad"}},
				},
			},
			wantErr: "spec.profile.resources.limits.cpu",
		},
		{
			name: "ateTemplate.atespace invalid name",
			spec: &v1alpha1.WorkspaceSpec{
				Profile: &v1alpha1.Profile{AteTemplate: &v1alpha1.ObjectRef{Name: "base-template", Atespace: "Bad_Space"}},
			},
			wantErr: "spec.profile.ateTemplate.atespace",
		},
		{
			name: "ateTemplate.name empty",
			spec: &v1alpha1.WorkspaceSpec{
				Profile: &v1alpha1.Profile{AteTemplate: &v1alpha1.ObjectRef{Atespace: "shared"}},
			},
			wantErr: "spec.profile.ateTemplate.name: must be set",
		},
		{
			name: "invalid_ateTemplate.name",
			spec: &v1alpha1.WorkspaceSpec{
				Profile: &v1alpha1.Profile{AteTemplate: &v1alpha1.ObjectRef{Name: "Bad_Name"}},
			},
			wantErr: "spec.profile.ateTemplate.name",
		},
		{
			name: "ateTemplate.name_combined_with_image",
			spec: &v1alpha1.WorkspaceSpec{
				Profile: &v1alpha1.Profile{
					AteTemplate: &v1alpha1.ObjectRef{Name: "base-template"},
					Image:       "us-docker.pkg.dev/p/img@sha256:abc",
				},
			},
			wantErr: "cannot be combined with image or resources",
		},
		{
			name: "ateTemplate.name_combined_with_resources",
			spec: &v1alpha1.WorkspaceSpec{
				Profile: &v1alpha1.Profile{
					AteTemplate: &v1alpha1.ObjectRef{Name: "base-template"},
					Resources:   &v1alpha1.ResourcesSpec{Limits: &v1alpha1.ResourceList{Cpu: "1"}},
				},
			},
			wantErr: "cannot be combined with image or resources",
		},
		{
			name: "invalid_setup_gateway",
			spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Gateway: &v1alpha1.GatewayRef{Name: "Bad_Name"},
				},
			},
			wantErr: "spec.setup.gateway.name",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateWorkspace(&v1alpha1.Workspace{
				Metadata: &v1alpha1.ObjectMeta{Name: "valid-ws"},
				Spec:     tc.spec,
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateWorkspace() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateWorkspace_ProfileImage(t *testing.T) {
	img := &v1alpha1.Profile{Image: "us-docker.pkg.dev/p/img@sha256:abc"}
	mcp := &v1alpha1.MCPConfig{Servers: []*v1alpha1.MCPServer{{Name: "gh", Endpoint: "https://example.com/mcp"}}}
	tests := []struct {
		name    string
		spec    *v1alpha1.WorkspaceSpec
		wantErr bool
	}{
		{name: "nil_spec"},
		{name: "mcp_only", spec: &v1alpha1.WorkspaceSpec{Mcp: mcp}},
		{name: "setup_with_image", spec: &v1alpha1.WorkspaceSpec{Profile: img, Setup: &v1alpha1.WorkspaceSetup{Commands: []string{"npm ci"}}}},
		{name: "ateTemplate", spec: &v1alpha1.WorkspaceSpec{Profile: &v1alpha1.Profile{AteTemplate: &v1alpha1.ObjectRef{Name: "tpl"}}}},
		{name: "setup_without_profile", spec: &v1alpha1.WorkspaceSpec{Setup: &v1alpha1.WorkspaceSetup{Commands: []string{"npm ci"}}}, wantErr: true},
		{name: "profile_without_image", spec: &v1alpha1.WorkspaceSpec{Profile: &v1alpha1.Profile{Resources: &v1alpha1.ResourcesSpec{Limits: &v1alpha1.ResourceList{Cpu: "2"}}}}, wantErr: true},
		{name: "setup_gateway_only", spec: &v1alpha1.WorkspaceSpec{Setup: &v1alpha1.WorkspaceSetup{Gateway: &v1alpha1.GatewayRef{Name: "gw"}}}},
		{name: "git_without_profile", spec: &v1alpha1.WorkspaceSpec{Setup: &v1alpha1.WorkspaceSetup{Git: []*v1alpha1.GitRepo{{Repo: "https://github.com/google/ax"}}}}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateWorkspace(&v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "ws"}, Spec: tc.spec})
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "spec.profile: image or ateTemplate.name is required") {
					t.Fatalf("ValidateWorkspace() = %v, want image required", err)
				}
			} else if err != nil {
				t.Fatalf("ValidateWorkspace() = %v, want nil", err)
			}
		})
	}
}
