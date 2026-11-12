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

package workspace_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/ax/internal/workspace"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

func setTestAXDir(t *testing.T, dir string) string {
	t.Helper()
	orig := workspace.AXDir
	workspace.AXDir = dir
	t.Cleanup(func() { workspace.AXDir = orig })
	return dir
}

func TestSetupWorkspace_MaidenRun(t *testing.T) {
	tempDir := t.TempDir()
	setTestAXDir(t, filepath.Join(tempDir, "ax-state"))

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "test-ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Setup: &v1alpha1.WorkspaceSetup{
				Skills: &v1alpha1.SkillsConfig{
					Path: filepath.Join(tempDir, "skills"),
				},
			},
		},
	}

	// 1. Maiden run
	res1, err := workspace.SetupWorkspace(t.Context(), ws, tempDir)
	if err != nil {
		t.Fatalf("SetupWorkspace() maiden run failed: %v", err)
	}
	if !res1.IsMaidenRun {
		t.Errorf("IsMaidenRun = false, want true")
	}
	if res1.SkillsMounted != filepath.Join(tempDir, "skills") {
		t.Errorf("SkillsMounted = %q, want %q", res1.SkillsMounted, filepath.Join(tempDir, "skills"))
	}

	markerFile := filepath.Join(tempDir, ".ax", workspace.InitializedMarkerFilename)
	if _, err := os.Stat(markerFile); os.IsNotExist(err) {
		t.Errorf("marker file %s missing", markerFile)
	}

	// 2. Second run detects marker and skips setup
	res2, err := workspace.SetupWorkspace(t.Context(), ws, tempDir)
	if err != nil {
		t.Fatalf("SetupWorkspace() second run failed: %v", err)
	}
	if res2.IsMaidenRun {
		t.Errorf("IsMaidenRun = true on second run, want false")
	}
}

func TestSetupWorkspace_DefaultSkillsPath(t *testing.T) {
	tempDir := t.TempDir()
	wsDir := filepath.Join(tempDir, "ws")
	setTestAXDir(t, filepath.Join(tempDir, "ax-state"))

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "skills-default-ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Setup: &v1alpha1.WorkspaceSetup{
				Skills: &v1alpha1.SkillsConfig{
					Registries: []*v1alpha1.SkillRegistry{{Provider: "ax", Query: "coding"}},
				},
			},
		},
	}

	res, err := workspace.SetupWorkspace(t.Context(), ws, wsDir)
	if err != nil {
		t.Fatalf("SetupWorkspace() failed: %v", err)
	}
	wantPath := filepath.Join(wsDir, ".ax", "skills")
	if res.SkillsMounted != wantPath {
		t.Fatalf("SkillsMounted = %q, want %q", res.SkillsMounted, wantPath)
	}
	if st, err := os.Stat(wantPath); err != nil || !st.IsDir() {
		t.Fatalf("default skills dir %s not created: %v", wantPath, err)
	}
}

func TestSetupWorkspace_GitSubdir(t *testing.T) {
	tempDir := t.TempDir()
	srcDir := filepath.Join(tempDir, "src")
	initSourceRepo(t, srcDir, "hello\n")

	wsDir := filepath.Join(tempDir, "ws")
	stateDir := setTestAXDir(t, filepath.Join(tempDir, "ax-state"))

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "test-git-ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Setup: &v1alpha1.WorkspaceSetup{
				Git: []*v1alpha1.GitRepo{{
					Name: "ax",
					Repo: srcDir,
				}},
			},
		},
	}

	res, err := workspace.SetupWorkspace(t.Context(), ws, wsDir)
	if err != nil {
		t.Fatalf("SetupWorkspace() failed: %v", err)
	}
	if len(res.ClonedRepos) != 1 {
		t.Fatalf("len(ClonedRepos) = %d, want 1", len(res.ClonedRepos))
	}

	clonedGit := filepath.Join(wsDir, "ax", ".git")
	if _, err := os.Stat(clonedGit); os.IsNotExist(err) {
		t.Errorf("cloned .git dir %s missing", clonedGit)
	}
	logFile := filepath.Join(stateDir, "git-success.log")
	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		t.Errorf("git-success.log %s missing", logFile)
	}
}

func TestSetupWorkspace_GitRefBranchTagAndCommitSHA(t *testing.T) {
	tempDir := t.TempDir()
	srcDir := filepath.Join(tempDir, "src")
	initCmd := exec.Command("sh", "-c", `
mkdir -p "`+srcDir+`" && cd "`+srcDir+`" &&
git init -q &&
git symbolic-ref HEAD refs/heads/main &&
git config user.email t@ax.io &&
git config user.name AX &&
echo v1 > file.txt && git add . && git commit -qm "c1" && git tag v1.0.0 && git branch release &&
echo v2 > file.txt && git add . && git commit -qm "c2"
`)
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("init repo: %v (%s)", err, out)
	}

	shaCmd := exec.Command("git", "rev-parse", "v1.0.0")
	shaCmd.Dir = srcDir
	c1Raw, err := shaCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	c1SHA := strings.TrimSpace(string(c1Raw))

	setTestAXDir(t, filepath.Join(tempDir, "ax-state"))

	tests := []struct {
		name string
		ref  string
	}{
		{name: "branch", ref: "release"},
		{name: "tag", ref: "v1.0.0"},
		{name: "short_sha", ref: c1SHA[:8]},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wsDir := filepath.Join(tempDir, "ws-"+tc.name)
			ws := &v1alpha1.Workspace{
				Metadata: &v1alpha1.ObjectMeta{Name: "ref-" + tc.name},
				Spec: &v1alpha1.WorkspaceSpec{
					Setup: &v1alpha1.WorkspaceSetup{
						Git: []*v1alpha1.GitRepo{{Repo: srcDir, Dir: ".", Ref: tc.ref}},
					},
				},
			}
			if _, err := workspace.SetupWorkspace(t.Context(), ws, wsDir); err != nil {
				t.Fatalf("SetupWorkspace(ref=%s) failed: %v", tc.ref, err)
			}
			headCmd := exec.Command("git", "rev-parse", "HEAD")
			headCmd.Dir = wsDir
			gotRaw, err := headCmd.Output()
			if err != nil {
				t.Fatalf("git rev-parse HEAD failed: %v", err)
			}
			if got := strings.TrimSpace(string(gotRaw)); got != c1SHA {
				t.Errorf("HEAD = %q, want %q", got, c1SHA)
			}
			data, _ := os.ReadFile(filepath.Join(wsDir, "file.txt"))
			if got := strings.TrimSpace(string(data)); got != "v1" {
				t.Errorf("file.txt = %q, want %q", got, "v1")
			}
		})
	}
}

func TestSetupWorkspace_GitError(t *testing.T) {
	tempDir := t.TempDir()
	wsDir := filepath.Join(tempDir, "ws")
	stateDir := setTestAXDir(t, filepath.Join(tempDir, "ax-state"))

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "test-git-err-ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Setup: &v1alpha1.WorkspaceSetup{
				Git: []*v1alpha1.GitRepo{{
					Name: "invalid-repo",
					Repo: "https://127.0.0.1:9/nonexistent/repo.git",
				}},
			},
		},
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	if _, err := workspace.SetupWorkspace(ctx, ws, wsDir); err == nil {
		t.Fatalf("SetupWorkspace() = nil, want error on failed git clone")
	}
	if _, err := os.Stat(filepath.Join(stateDir, "git-error.log")); os.IsNotExist(err) {
		t.Errorf("git-error.log missing under stateDir")
	}
	if _, err := os.Stat(filepath.Join(wsDir, ".ax", workspace.InitializedMarkerFilename)); !os.IsNotExist(err) {
		t.Errorf("marker file created despite failed git clone")
	}
}

func TestSetupWorkspace_GitDepth(t *testing.T) {
	tempDir := t.TempDir()
	srcDir := filepath.Join(tempDir, "src")
	wsDir := filepath.Join(tempDir, "ws")
	setTestAXDir(t, filepath.Join(tempDir, "ax-state"))

	initCmd := exec.Command("sh", "-c", `
mkdir -p "`+srcDir+`" && cd "`+srcDir+`" &&
git init -q &&
git symbolic-ref HEAD refs/heads/main &&
git config user.email 'test@ax.io' &&
git config user.name 'AX' &&
echo '1' > file.txt && git add . && git commit -qm 'commit 1' &&
echo '2' > file.txt && git add . && git commit -qm 'commit 2' &&
echo '3' > file.txt && git add . && git commit -qm 'commit 3'
`)
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("init test git repo: %v (%s)", err, out)
	}

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "test-git-depth-ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Setup: &v1alpha1.WorkspaceSetup{
				Git: []*v1alpha1.GitRepo{{
					Name:  "shallow",
					Repo:  "file://" + srcDir,
					Depth: 1,
				}},
			},
		},
	}

	res, err := workspace.SetupWorkspace(t.Context(), ws, wsDir)
	if err != nil {
		t.Fatalf("SetupWorkspace() failed: %v", err)
	}
	if len(res.ClonedRepos) != 1 {
		t.Errorf("len(ClonedRepos) = %d, want 1", len(res.ClonedRepos))
	}

	countCmd := exec.Command("git", "rev-list", "--count", "HEAD")
	countCmd.Dir = filepath.Join(wsDir, "shallow")
	out, err := countCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-list failed: %v (%s)", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "1" {
		t.Errorf("shallow commit count = %q, want %q", got, "1")
	}
}

func TestSetupWorkspace_Commands(t *testing.T) {
	tempDir := t.TempDir()
	setTestAXDir(t, filepath.Join(tempDir, "ax-state"))

	t.Run("success", func(t *testing.T) {
		okDir := filepath.Join(tempDir, "ok")
		okWS := &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: "cmd-ok"},
			Spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Files: []*v1alpha1.File{{Path: "deps.txt", Content: "pkg-a\n"}},
					Commands: []string{
						"cat deps.txt > installed.txt",
						"echo done >> installed.txt",
					},
				},
			},
		}
		res, err := workspace.SetupWorkspace(t.Context(), okWS, okDir)
		if err != nil {
			t.Fatalf("SetupWorkspace() failed: %v", err)
		}
		if res.CommandsRun != 2 {
			t.Errorf("CommandsRun = %d, want 2", res.CommandsRun)
		}
		got, err := os.ReadFile(filepath.Join(okDir, "installed.txt"))
		if err != nil || string(got) != "pkg-a\ndone\n" {
			t.Errorf("installed.txt = %q (%v), want %q", got, err, "pkg-a\ndone\n")
		}
	})

	t.Run("failure_withholds_marker", func(t *testing.T) {
		failDir := filepath.Join(tempDir, "fail")
		failWS := &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: "cmd-fail"},
			Spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Commands: []string{"echo boom >&2 && exit 7"},
				},
			},
		}
		_, err := workspace.SetupWorkspace(t.Context(), failWS, failDir)
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("SetupWorkspace() = %v, want error containing %q", err, "boom")
		}
		if _, statErr := os.Stat(filepath.Join(failDir, ".ax", workspace.InitializedMarkerFilename)); !os.IsNotExist(statErr) {
			t.Errorf("marker created despite failing setup command")
		}
	})
}

func TestMarkerName(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{path: "/workspace", want: "initialized-workspace"},
		{path: "/workspace/", want: "initialized-workspace"},
		{path: "", want: "initialized-workspace"},
		{path: "/", want: "initialized-root"},
		{path: "/workspace/tools", want: "initialized-workspace-tools"},
		{path: "/workspace/a/b", want: "initialized-workspace-a-b"},
		{path: "/srv/data", want: "initialized-srv-data"},
		{path: "/workspace/tools/", want: "initialized-workspace-tools"},
	}
	for _, tc := range tests {
		if got := workspace.MarkerName(tc.path); got != tc.want {
			t.Errorf("MarkerName(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestSetupWorkspace_InlinedFiles(t *testing.T) {
	tempDir := t.TempDir()
	wsDir := filepath.Join(tempDir, "ws")
	setTestAXDir(t, filepath.Join(tempDir, "ax-state"))

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "files-ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Setup: &v1alpha1.WorkspaceSetup{
				Files: []*v1alpha1.File{
					{Path: "config/settings.json", Content: `{"env": "production"}`},
					{Path: "notes.txt", Content: "Hello AX"},
				},
			},
		},
	}

	res, err := workspace.SetupWorkspace(t.Context(), ws, wsDir)
	if err != nil {
		t.Fatalf("SetupWorkspace() failed: %v", err)
	}
	if !res.IsMaidenRun {
		t.Errorf("IsMaidenRun = false, want true")
	}

	configData, err := os.ReadFile(filepath.Join(wsDir, "config", "settings.json"))
	if err != nil || string(configData) != `{"env": "production"}` {
		t.Errorf("config/settings.json = %q (%v), want %q", configData, err, `{"env": "production"}`)
	}
	notesData, err := os.ReadFile(filepath.Join(wsDir, "notes.txt"))
	if err != nil || string(notesData) != "Hello AX" {
		t.Errorf("notes.txt = %q (%v), want %q", notesData, err, "Hello AX")
	}
}

// TestSetupWorkspace_MarkerFollowsRootClone checks that when a repo is cloned into the
// workspace root, the marker is written where the next run looks for it: inside the new
// .git directory. The next run must then skip setup.
func TestSetupWorkspace_MarkerFollowsRootClone(t *testing.T) {
	tempDir := t.TempDir()
	src := filepath.Join(tempDir, "src")
	initSourceRepo(t, src, "hi\n")

	target := filepath.Join(tempDir, "ws")
	setTestAXDir(t, filepath.Join(tempDir, "ax-state-root-clone"))

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "root-clone-ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Setup: &v1alpha1.WorkspaceSetup{
				Git: []*v1alpha1.GitRepo{{Name: "root", Repo: src, Dir: "."}},
			},
		},
	}
	if _, err := workspace.SetupWorkspace(t.Context(), ws, target); err != nil {
		t.Fatalf("SetupWorkspace() first run failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, ".git", ".ax", workspace.InitializedMarkerFilename)); err != nil {
		t.Fatalf("marker not in the cloned .git directory: %v", err)
	}
	res, err := workspace.SetupWorkspace(t.Context(), ws, target)
	if err != nil {
		t.Fatalf("SetupWorkspace() second run failed: %v", err)
	}
	if res.IsMaidenRun {
		t.Errorf("IsMaidenRun = true on second run, want false")
	}
}

// initSourceRepo makes a git repo at dir with one committed file, README.md = content.
func initSourceRepo(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "git init -q && git symbolic-ref HEAD refs/heads/main && git config user.email t@ax.io && git config user.name AX && git add . && git commit -qm init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
}

// TestSetupWorkspace_MigratesOwnLegacyMarker checks that a legacy marker written for this
// workspace is migrated, so setup does not rerun.
func TestSetupWorkspace_MigratesOwnLegacyMarker(t *testing.T) {
	tempDir := t.TempDir()
	target := filepath.Join(tempDir, "ws")
	axDir := setTestAXDir(t, filepath.Join(tempDir, "ax-state-own"))
	if err := os.MkdirAll(axDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(axDir, workspace.MarkerName(target)), []byte("workspace: own-ws\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "own-ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Setup: &v1alpha1.WorkspaceSetup{
				Files: []*v1alpha1.File{{Path: "x.txt", Content: "x"}},
			},
		},
	}
	if _, err := workspace.SetupWorkspace(t.Context(), ws, target); err != nil {
		t.Fatalf("SetupWorkspace() failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, ".ax", workspace.InitializedMarkerFilename)); err != nil {
		t.Errorf("legacy marker not migrated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "x.txt")); !os.IsNotExist(err) {
		t.Errorf("setup reran although the legacy marker belonged to this workspace")
	}
}

// TestSetupWorkspace_IgnoresLegacyMarkerOfOtherWorkspace checks that a legacy marker written
// for a different workspace, with the same legacy name, does not suppress this setup.
func TestSetupWorkspace_IgnoresLegacyMarkerOfOtherWorkspace(t *testing.T) {
	tempDir := t.TempDir()
	target := filepath.Join(tempDir, "ws")
	axDir := setTestAXDir(t, filepath.Join(tempDir, "ax-state-other"))
	if err := os.MkdirAll(axDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(axDir, workspace.MarkerName(target)), []byte("workspace: other-ws\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "this-ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Setup: &v1alpha1.WorkspaceSetup{
				Files: []*v1alpha1.File{{Path: "x.txt", Content: "x"}},
			},
		},
	}
	if _, err := workspace.SetupWorkspace(t.Context(), ws, target); err != nil {
		t.Fatalf("SetupWorkspace() failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "x.txt")); err != nil {
		t.Errorf("setup was skipped because of another workspace's legacy marker: %v", err)
	}
}

// TestSetupWorkspace_RefusesLinkedMarkerDir checks that a .ax directory that is a link to a
// directory outside the workspace is refused, and nothing is written through it.
func TestSetupWorkspace_RefusesLinkedMarkerDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links needs extra privileges on Windows")
	}
	tempDir := t.TempDir()
	outside := filepath.Join(tempDir, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(tempDir, "ws")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(target, ".ax")); err != nil {
		t.Fatal(err)
	}
	setTestAXDir(t, filepath.Join(tempDir, "ax-state-linked"))

	ws := &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "linked-ws"}}
	if _, err := workspace.SetupWorkspace(t.Context(), ws, target); err == nil {
		t.Errorf("SetupWorkspace() = nil, want error for linked .ax directory")
	}
	if _, err := os.Stat(filepath.Join(outside, workspace.InitializedMarkerFilename)); !os.IsNotExist(err) {
		t.Errorf("marker was written through a linked .ax directory")
	}
}

func TestSetupWorkspace_RejectsEscapingPaths(t *testing.T) {
	tempDir := t.TempDir()
	setTestAXDir(t, filepath.Join(tempDir, "ax-state"))

	t.Run("git_name_escape", func(t *testing.T) {
		ws := &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: "escape-ws"},
			Spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Git: []*v1alpha1.GitRepo{{Name: "../escaped-repo", Repo: "https://example.com/r.git"}},
				},
			},
		}
		if _, err := workspace.SetupWorkspace(t.Context(), ws, filepath.Join(tempDir, "ws-git")); err == nil {
			t.Fatal("SetupWorkspace() = nil, want error on escaping git.name")
		}
	})

	t.Run("dot_file_rejected", func(t *testing.T) {
		ws := &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: "dot-file"},
			Spec: &v1alpha1.WorkspaceSpec{
				Setup: &v1alpha1.WorkspaceSetup{
					Files: []*v1alpha1.File{{Path: ".", Content: "x"}},
				},
			},
		}
		_, err := workspace.SetupWorkspace(t.Context(), ws, filepath.Join(tempDir, "ws-dot"))
		if err == nil || !strings.Contains(err.Error(), "must name a file") {
			t.Fatalf("SetupWorkspace() = %v, want error containing %q", err, "must name a file")
		}
	})
}
