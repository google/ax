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
	"testing"
	"time"

	"github.com/google/ax/internal/workspace"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

func TestSetupWorkspace_MaidenRun(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ax-ws-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{
			Name: "test-ws",
		},
		Spec: &v1alpha1.WorkspaceSpec{
			Skills: &v1alpha1.SkillsConfig{
				Path: filepath.Join(tempDir, "skills"),
			},
		},
	}

	stateDir := filepath.Join(tempDir, "ax-state")
	origAXDir := workspace.AXDir
	workspace.AXDir = stateDir
	defer func() { workspace.AXDir = origAXDir }()

	// 1. Maiden run
	res1, err := workspace.SetupWorkspace(context.Background(), ws, tempDir)
	if err != nil {
		t.Fatalf("maiden setup failed: %v", err)
	}
	if !res1.IsMaidenRun {
		t.Errorf("expected maiden run to be true")
	}

	// Verify marker file exists under the workspace .ax directory
	markerFile := filepath.Join(tempDir, ".ax", workspace.InitializedMarkerFilename)
	if _, err := os.Stat(markerFile); os.IsNotExist(err) {
		t.Errorf("expected marker file %s to exist", markerFile)
	}

	// The old marker name must not be written into the workspace.
	if _, err := os.Stat(filepath.Join(tempDir, ".ax-initialized")); !os.IsNotExist(err) {
		t.Errorf("expected .ax-initialized not to be in workspace")
	}

	// 2. Second run (should detect already initialized)
	res2, err := workspace.SetupWorkspace(context.Background(), ws, tempDir)
	if err != nil {
		t.Fatalf("second setup failed: %v", err)
	}
	if res2.IsMaidenRun {
		t.Errorf("expected maiden run to be false on second run")
	}
}

func TestSetupWorkspace_GitSubdir(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ax-ws-git-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create a dummy source git repository
	srcDir, err := os.MkdirTemp("", "ax-src-git-*")
	if err != nil {
		t.Fatalf("failed to create src dir: %v", err)
	}
	defer os.RemoveAll(srcDir)

	// Init source repo
	// git init -b requires git 2.28+; symbolic-ref works on any version.
	initCmd := exec.Command("sh", "-c", "git init && git symbolic-ref HEAD refs/heads/main && git config user.email 'test@ax.io' && git config user.name 'AX' && echo 'hello' > README.md && git add . && git commit -m 'initial'")
	initCmd.Dir = srcDir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to init test git repo: %v (%s)", err, string(out))
	}

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{
			Name: "test-git-ws",
		},
		Spec: &v1alpha1.WorkspaceSpec{
			Git: []*v1alpha1.GitRepo{
				{
					Name: "ax",
					Repo: srcDir,
				},
			},
		},
	}

	stateDir := filepath.Join(tempDir, "ax-state")
	origAXDir := workspace.AXDir
	workspace.AXDir = stateDir
	defer func() { workspace.AXDir = origAXDir }()

	res, err := workspace.SetupWorkspace(context.Background(), ws, tempDir)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if len(res.ClonedRepos) != 1 {
		t.Errorf("expected 1 cloned repo, got %d", len(res.ClonedRepos))
	}

	// Verify repo was cloned into subdirectory 'ax' inside tempDir
	clonedGit := filepath.Join(tempDir, "ax", ".git")
	if _, err := os.Stat(clonedGit); os.IsNotExist(err) {
		t.Errorf("expected %s to exist", clonedGit)
	}

	// Verify git-success.log is created under stateDir and NOT inside workspace
	logFile := filepath.Join(stateDir, "git-success.log")
	if _, err := os.Stat(logFile); os.IsNotExist(err) {
		t.Errorf("expected git log file %s to exist in stateDir", logFile)
	}
	if _, err := os.Stat(filepath.Join(tempDir, ".ax-git-success.log")); !os.IsNotExist(err) {
		t.Errorf("expected .ax-git-success.log NOT to exist in workspace")
	}
}

func TestSetupWorkspace_GitError(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ax-ws-git-err-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{
			Name: "test-git-err-ws",
		},
		Spec: &v1alpha1.WorkspaceSpec{
			Git: []*v1alpha1.GitRepo{
				{
					Name: "invalid-repo",
					Repo: "https://127.0.0.1:9/nonexistent/repo.git",
				},
			},
		},
	}

	stateDir := filepath.Join(tempDir, "ax-state")
	origAXDir := workspace.AXDir
	workspace.AXDir = stateDir
	defer func() { workspace.AXDir = origAXDir }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, _ = workspace.SetupWorkspace(ctx, ws, tempDir)

	// Verify git-error.log is created under stateDir and NOT inside workspace
	errLog := filepath.Join(stateDir, "git-error.log")
	if _, err := os.Stat(errLog); os.IsNotExist(err) {
		t.Errorf("expected git error log %s to exist under stateDir", errLog)
	}

	// Verify workspace directory does not have .ax-git-error.log or .ax
	if _, err := os.Stat(filepath.Join(tempDir, ".ax-git-error.log")); !os.IsNotExist(err) {
		t.Errorf("expected .ax-git-error.log NOT to exist in workspace")
	}
	if _, err := os.Stat(filepath.Join(tempDir, ".ax")); !os.IsNotExist(err) {
		t.Errorf("expected .ax directory NOT to exist in workspace")
	}

	// Verify initialization marker was not created due to failure
	markerFile := filepath.Join(tempDir, ".ax", workspace.InitializedMarkerFilename)
	if _, err := os.Stat(markerFile); !os.IsNotExist(err) {
		t.Errorf("expected marker file %s NOT to exist after failure", markerFile)
	}
}

func TestSetupWorkspace_GitDepth(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ax-ws-git-depth-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create a dummy source git repository with 3 commits
	srcDir, err := os.MkdirTemp("", "ax-src-git-*")
	if err != nil {
		t.Fatalf("failed to create src dir: %v", err)
	}
	defer os.RemoveAll(srcDir)

	initCmd := exec.Command("sh", "-c", `
git init &&
git symbolic-ref HEAD refs/heads/main &&
git config user.email 'test@ax.io' &&
git config user.name 'AX' &&
echo '1' > file.txt && git add . && git commit -m 'commit 1' &&
echo '2' > file.txt && git add . && git commit -m 'commit 2' &&
echo '3' > file.txt && git add . && git commit -m 'commit 3'
`)
	initCmd.Dir = srcDir
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to init test git repo with commits: %v (%s)", err, string(out))
	}

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{
			Name: "test-shallow-git-ws",
		},
		Spec: &v1alpha1.WorkspaceSpec{
			Git: []*v1alpha1.GitRepo{
				{
					Name:  "ax",
					Repo:  srcDir,
					Depth: 1,
				},
			},
		},
	}

	stateDir := filepath.Join(tempDir, "ax-state")
	origAXDir := workspace.AXDir
	workspace.AXDir = stateDir
	defer func() { workspace.AXDir = origAXDir }()

	res, err := workspace.SetupWorkspace(context.Background(), ws, tempDir)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if len(res.ClonedRepos) != 1 {
		t.Errorf("expected 1 cloned repo, got %d", len(res.ClonedRepos))
	}

	// Verify commit count in cloned repo is exactly 1
	countCmd := exec.Command("git", "rev-list", "--count", "HEAD")
	countCmd.Dir = filepath.Join(tempDir, "ax")
	out, err := countCmd.Output()
	if err != nil {
		t.Fatalf("failed to count commits: %v", err)
	}
	if got := string(out); got != "1\n" && got != "1" {
		t.Errorf("expected 1 commit with depth=1, got %q", got)
	}
}

func TestMarkerName(t *testing.T) {
	tests := map[string]string{
		"/workspace":        "initialized-workspace",
		"/workspace/":       "initialized-workspace",
		"":                  "initialized-workspace",
		"/":                 "initialized-root",
		"/workspace/tools":  "initialized-workspace-tools",
		"/workspace/a/b":    "initialized-workspace-a-b",
		"/srv/data":         "initialized-srv-data",
		"/workspace/tools/": "initialized-workspace-tools",
	}
	for path, want := range tests {
		if got := workspace.MarkerName(path); got != want {
			t.Errorf("MarkerName(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestSetupWorkspace_InlinedFiles(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "ax-ws-files-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "files-ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Files: []*v1alpha1.File{
				{Path: "config/settings.json", Content: `{"env": "production"}`},
				{Path: "notes.txt", Content: "Hello AX"},
			},
		},
	}

	stateDir := filepath.Join(tempDir, "ax-state")
	origAXDir := workspace.AXDir
	workspace.AXDir = stateDir
	defer func() { workspace.AXDir = origAXDir }()

	res, err := workspace.SetupWorkspace(context.Background(), ws, tempDir)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if !res.IsMaidenRun {
		t.Errorf("expected maiden run to be true")
	}

	configData, err := os.ReadFile(filepath.Join(tempDir, "config", "settings.json"))
	if err != nil {
		t.Fatalf("reading config/settings.json: %v", err)
	}
	if string(configData) != `{"env": "production"}` {
		t.Errorf("unexpected content in config/settings.json: %s", string(configData))
	}

	notesData, err := os.ReadFile(filepath.Join(tempDir, "notes.txt"))
	if err != nil {
		t.Fatalf("reading notes.txt: %v", err)
	}
	if string(notesData) != "Hello AX" {
		t.Errorf("unexpected content in notes.txt: %s", string(notesData))
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
	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-root-clone")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "root-clone-ws"},
		Spec:     &v1alpha1.WorkspaceSpec{Git: []*v1alpha1.GitRepo{{Name: "root", Repo: src, Dir: "."}}},
	}
	if _, err := workspace.SetupWorkspace(context.Background(), ws, target); err != nil {
		t.Fatalf("first setup failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, ".git", ".ax", workspace.InitializedMarkerFilename)); err != nil {
		t.Fatalf("marker not in the cloned .git directory: %v", err)
	}
	res, err := workspace.SetupWorkspace(context.Background(), ws, target)
	if err != nil {
		t.Fatalf("second setup failed: %v", err)
	}
	if res.IsMaidenRun {
		t.Errorf("second run reran setup; the marker was not found")
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
	axDir := filepath.Join(tempDir, "ax-state-own")
	if err := os.MkdirAll(axDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(axDir, workspace.MarkerName(target)), []byte("workspace: own-ws\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	origAXDir := workspace.AXDir
	workspace.AXDir = axDir
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "own-ws"},
		Spec:     &v1alpha1.WorkspaceSpec{Files: []*v1alpha1.File{{Path: "x.txt", Content: "x"}}},
	}
	if _, err := workspace.SetupWorkspace(context.Background(), ws, target); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, ".ax", workspace.InitializedMarkerFilename)); err != nil {
		t.Errorf("legacy marker not migrated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "x.txt")); !os.IsNotExist(err) {
		t.Errorf("setup reran although the legacy marker was this workspace's")
	}
}

// TestSetupWorkspace_IgnoresLegacyMarkerOfOtherWorkspace checks that a legacy marker written
// for a different workspace, with the same legacy name, does not suppress this setup.
func TestSetupWorkspace_IgnoresLegacyMarkerOfOtherWorkspace(t *testing.T) {
	tempDir := t.TempDir()
	target := filepath.Join(tempDir, "ws")
	axDir := filepath.Join(tempDir, "ax-state-other")
	if err := os.MkdirAll(axDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(axDir, workspace.MarkerName(target)), []byte("workspace: other-ws\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	origAXDir := workspace.AXDir
	workspace.AXDir = axDir
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "this-ws"},
		Spec:     &v1alpha1.WorkspaceSpec{Files: []*v1alpha1.File{{Path: "x.txt", Content: "x"}}},
	}
	if _, err := workspace.SetupWorkspace(context.Background(), ws, target); err != nil {
		t.Fatalf("setup failed: %v", err)
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
	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(tempDir, "ax-state-linked")
	defer func() { workspace.AXDir = origAXDir }()

	ws := &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "linked-ws"}}
	if _, err := workspace.SetupWorkspace(context.Background(), ws, target); err == nil {
		t.Errorf("expected a linked .ax directory to be refused")
	}
	if _, err := os.Stat(filepath.Join(outside, workspace.InitializedMarkerFilename)); !os.IsNotExist(err) {
		t.Errorf("marker was written through a linked .ax directory")
	}
}
