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

package apivalidation

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

// ValidateWorkspace reports the first problem with a workspace that would make
// it unusable. It is called by the API server before saving.
func ValidateWorkspace(w *v1alpha1.Workspace) error {
	if err := ValidateObjectMeta(w.GetMetadata()); err != nil {
		return err
	}
	spec := w.GetSpec()
	if spec == nil {
		return nil
	}
	if err := validateProfile("spec.profile", spec.GetProfile()); err != nil {
		return err
	}
	if err := validateSetup(spec.GetSetup()); err != nil {
		return err
	}
	return validateMCP(spec.GetMcp())
}

func validateSetup(setup *v1alpha1.WorkspaceSetup) error {
	if setup == nil {
		return nil
	}
	for i, g := range setup.GetGit() {
		if err := validateGitRepo(fmt.Sprintf("spec.setup.git[%d]", i), g); err != nil {
			return err
		}
	}
	for i, f := range setup.GetFiles() {
		if err := validateSetupFile(fmt.Sprintf("spec.setup.files[%d]", i), f); err != nil {
			return err
		}
	}
	if err := validateSkills(setup.GetSkills()); err != nil {
		return err
	}
	return validateGatewayRef("spec.setup.gateway", setup.GetGateway())
}

func validateGitRepo(field string, g *v1alpha1.GitRepo) error {
	repo := g.GetRepo()
	if strings.TrimSpace(repo) == "" {
		return fmt.Errorf("%s: repo is required", field)
	}
	if hasInvalidGitArg(repo) {
		return fmt.Errorf("%s.repo: invalid repository %q", field, repo)
	}
	if lower := strings.ToLower(repo); strings.HasPrefix(lower, "git@") || strings.HasPrefix(lower, "ssh://") {
		return fmt.Errorf("%s.repo: SSH git URLs (%q) are not supported; use HTTPS", field, repo)
	}

	if ref := g.GetRef(); ref != "" && hasInvalidGitArg(ref) {
		return fmt.Errorf("%s.ref: invalid ref %q", field, ref)
	}
	if g.GetDepth() < 0 {
		return fmt.Errorf("%s.depth: must be greater than or equal to 0", field)
	}

	if name := g.GetName(); name != "" {
		if err := validateRelativePath(field+".name", name); err != nil {
			return err
		}
		if filepath.Clean(name) == "." {
			return fmt.Errorf("%s.name: %q is invalid; set dir: \".\" to clone into the workspace root", field, name)
		}
	}

	return validateRelativePath(field+".dir", g.GetDir())
}

func validateSetupFile(field string, f *v1alpha1.File) error {
	p := f.GetPath()
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("%s: path is required", field)
	}
	if err := validateRelativePath(field+".path", p); err != nil {
		return err
	}
	if strings.HasSuffix(p, "/") || filepath.Clean(p) == "." {
		return fmt.Errorf("%s.path: %q must name a file, not a directory", field, p)
	}
	return nil
}

func validateSkills(skills *v1alpha1.SkillsConfig) error {
	if p := skills.GetPath(); p != "" && !filepath.IsAbs(p) && escapesWorkspaceRoot(p) {
		return fmt.Errorf("spec.setup.skills.path: %q must not escape the workspace root", p)
	}
	return nil
}

func validateMCP(mcp *v1alpha1.MCPConfig) error {
	if mcp == nil {
		return nil
	}
	for i, srv := range mcp.GetServers() {
		if err := validateGatewayRef(fmt.Sprintf("spec.mcp.servers[%d].gateway", i), srv.GetGateway()); err != nil {
			return err
		}
	}
	for i, reg := range mcp.GetRegistries() {
		for j, srv := range reg.GetServers() {
			if err := validateGatewayRef(fmt.Sprintf("spec.mcp.registries[%d].servers[%d].gateway", i, j), srv.GetGateway()); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasInvalidGitArg(s string) bool {
	return strings.HasPrefix(s, "-") || strings.ContainsAny(s, " \t\r\n\x00")
}

func validateRelativePath(field, p string) error {
	if p == "" {
		return nil
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("%s: %q must be relative to the workspace root", field, p)
	}
	if escapesWorkspaceRoot(p) {
		return fmt.Errorf("%s: %q must not escape the workspace root", field, p)
	}
	return nil
}

func escapesWorkspaceRoot(relPath string) bool {
	clean := filepath.ToSlash(filepath.Clean(relPath))
	return clean == ".." || strings.HasPrefix(clean, "../")
}
