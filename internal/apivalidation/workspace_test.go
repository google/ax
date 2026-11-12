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
)

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
					AteActorTemplate: "base-template",
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
				Setup: &v1alpha1.WorkspaceSetup{
					Git: []*v1alpha1.GitRepo{{Repo: "/opt/e2e-fixtures/sample-repo.git", Ref: "main"}},
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := apivalidation.ValidateWorkspace(&v1alpha1.Workspace{
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
			name: "invalid_ate_actor_template",
			spec: &v1alpha1.WorkspaceSpec{
				Profile: &v1alpha1.Profile{AteActorTemplate: "Bad_Name"},
			},
			wantErr: "spec.profile.ateActorTemplate",
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
			err := apivalidation.ValidateWorkspace(&v1alpha1.Workspace{
				Metadata: &v1alpha1.ObjectMeta{Name: "valid-ws"},
				Spec:     tc.spec,
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateWorkspace() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}
