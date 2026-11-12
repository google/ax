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

package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

const (
	// DefaultAXDir is the default directory inside the container used for AX system state and logs.
	DefaultAXDir = "/ax"
	// InitializedMarkerFilename is the base name of the marker file written after a successful maiden run.
	InitializedMarkerFilename = "initialized"
	// defaultSetupCommandTimeout bounds a single spec.setup.commands entry so a
	// hung command fails setup instead of leaving the workspace unready forever.
	defaultSetupCommandTimeout = 5 * time.Minute
	// markerDirName is the directory under the workspace that holds the marker and default skills.
	markerDirName       = ".ax"
	defaultSkillsSubdir = "skills"

	defaultWorkspacePath = "/workspace"

	maxCommandOutputInError = 2048

	dirPerm  = 0o755
	filePerm = 0o644
)

// AXDir is the directory inside the container used for AX system state and logs.
// Tests override it to keep state out of the workspace.
var AXDir = DefaultAXDir

// SetupResult contains details about the workspace maiden initialization.
//
// TODO: Once workspace setup runs only in dedicated setup mode rather than on
// every task container boot, drop IsMaidenRun and simplify or remove SetupResult.
type SetupResult struct {
	WorkspacePath string
	ClonedRepos   []string
	SkillsMounted string
	CommandsRun   int
	IsMaidenRun   bool
}

// SetupWorkspace prepares targetPath according to WorkspaceSpec.setup on its
// maiden run:
//  1. Clone declared Git repositories at their target ref.
//  2. Write inline setup files.
//  3. Create the skills directory.
//  4. Run setup commands sequentially.
//
// A marker file under targetPath (see workspaceMarkerPath) records a completed
// maiden run so subsequent calls are no-ops.
//
// Any failure during maiden setup omits the marker and returns an error so
// callers do not mark the workspace ready and the next start retries setup.
func SetupWorkspace(ctx context.Context, ws *v1alpha1.Workspace, targetPath string) (*SetupResult, error) {
	if targetPath == "" {
		targetPath = defaultWorkspacePath
	}
	// Tooling downstream (git) scopes itself to this path, so it must be absolute.
	if abs, err := filepath.Abs(targetPath); err == nil {
		targetPath = abs
	}
	res := &SetupResult{WorkspacePath: targetPath}

	if err := os.MkdirAll(targetPath, dirPerm); err != nil {
		return nil, fmt.Errorf("creating workspace directory %s: %w", targetPath, err)
	}
	if err := os.MkdirAll(AXDir, dirPerm); err != nil {
		slog.Warn("creating ax state directory", "dir", AXDir, "error", err)
	}

	markerPath := workspaceMarkerPath(targetPath)
	if markerLinked(markerPath) {
		return nil, fmt.Errorf("marker path %s is a symbolic link; refusing to trust or write it", markerPath)
	}
	if _, err := os.Stat(markerPath); err == nil {
		slog.Info("workspace already initialized; skipping maiden run setup", "path", targetPath)
		return res, nil
	}
	// A marker from before the workspace-volume change lives under AXDir. Migrate it, so a
	// container that still has AXDir does not rerun setup over the agent's edits.
	if data, err := os.ReadFile(filepath.Join(AXDir, MarkerName(targetPath))); err == nil && legacyOwnedBy(data, ws) {
		slog.Info("migrating legacy workspace marker", "path", targetPath)
		if err := writeMarker(workspaceMarkerPath(targetPath), ws); err != nil {
			return res, fmt.Errorf("writing initialization marker: %w", err)
		}
		return res, nil
	}

	slog.Info("executing workspace maiden run setup", "path", targetPath)
	res.IsMaidenRun = true

	if setup := ws.GetSpec().GetSetup(); setup != nil {
		cloned, err := cloneRepos(ctx, setup.GetGit(), targetPath)
		res.ClonedRepos = cloned
		if err != nil {
			return res, err
		}

		if err := writeFiles(setup.GetFiles(), targetPath); err != nil {
			return res, err
		}

		skillsPath, err := setupSkills(ctx, setup.GetSkills(), targetPath)
		if err != nil {
			return res, err
		}
		res.SkillsMounted = skillsPath

		ran, err := runSetupCommands(ctx, setup.GetCommands(), targetPath)
		res.CommandsRun = ran
		if err != nil {
			return res, err
		}
	}

	finalMarker := workspaceMarkerPath(targetPath)
	if markerLinked(finalMarker) {
		return res, fmt.Errorf("marker path %s is a symbolic link; refusing to trust or write it", finalMarker)
	}
	if err := writeMarker(finalMarker, ws); err != nil {
		return res, fmt.Errorf("writing initialization marker %s: %w", finalMarker, err)
	}
	slog.Info("maiden run workspace setup completed successfully", "path", targetPath)
	return res, nil
}

// setupSkills creates the skills directory if declared or if registries are
// configured, returning its resolved path.
func setupSkills(_ context.Context, skills *v1alpha1.SkillsConfig, targetPath string) (string, error) {
	if skills == nil {
		return "", nil
	}
	dest := skills.GetPath()
	switch {
	case dest == "":
		if len(skills.GetRegistries()) == 0 {
			return "", nil
		}
		dest = filepath.Join(targetPath, markerDirName, defaultSkillsSubdir)
	case !filepath.IsAbs(dest):
		if !isRelativeWithinRoot(dest) {
			return "", fmt.Errorf("skills path %q escapes workspace %s", dest, targetPath)
		}
		dest = filepath.Join(targetPath, dest)
	}
	if err := os.MkdirAll(dest, dirPerm); err != nil {
		return "", fmt.Errorf("creating skills dir %s: %w", dest, err)
	}
	if len(skills.GetRegistries()) > 0 {
		// TODO: Fetch and materialize SKILL.md bundles from external skill
		// registries (using the setup actor's setup.gateway egress policy) into dest.
		slog.Info("prepared skills directory; registry skill download not implemented yet", "path", dest, "registries", len(skills.GetRegistries()))
	}
	return dest, nil
}

// runSetupCommands executes deterministic setup commands sequentially inside
// targetPath. The first failing or timed-out command aborts setup.
func runSetupCommands(ctx context.Context, commands []string, targetPath string) (int, error) {
	ran := 0
	for i, cmdStr := range commands {
		if strings.TrimSpace(cmdStr) == "" {
			continue
		}
		slog.Info("running workspace setup command", "index", i, "command", cmdStr, "dir", targetPath)
		out, err := runSetupCommandWithTimeout(ctx, cmdStr, targetPath, defaultSetupCommandTimeout)
		if err != nil {
			if len(out) > maxCommandOutputInError {
				out = out[len(out)-maxCommandOutputInError:]
			}
			snippet := strings.ToValidUTF8(strings.TrimSpace(string(out)), "")
			if snippet != "" {
				return ran, fmt.Errorf("running setup.commands[%d] (%q): %s: %w", i, cmdStr, snippet, err)
			}
			return ran, fmt.Errorf("running setup.commands[%d] (%q): %w", i, cmdStr, err)
		}
		ran++
	}
	return ran, nil
}

func runSetupCommandWithTimeout(ctx context.Context, cmdStr, targetPath string, timeout time.Duration) ([]byte, error) {
	cmdCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		cmdCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(cmdCtx, "sh", "-c", cmdStr)
	cmd.Dir = targetPath
	// Put the shell in its own process group so cancelling on timeout kills
	// backgrounded children holding stdout/stderr open instead of hanging.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second

	out, err := cmd.CombinedOutput()
	if err != nil && errors.Is(cmdCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		return out, fmt.Errorf("timed out after %s: %w", timeout, cmdCtx.Err())
	}
	return out, err
}

// MarkerName returns the maiden-run marker file name for a workspace mounted at
// path. The name is derived from the path so a workspace mounted somewhere
// other than the default tracks its setup independently.
func MarkerName(path string) string {
	if path == "" {
		path = defaultWorkspacePath
	}
	clean := strings.Trim(filepath.ToSlash(filepath.Clean(path)), "/")
	if clean == "" {
		clean = "root"
	}
	return InitializedMarkerFilename + "-" + strings.ReplaceAll(clean, "/", "-")
}

// workspaceMarkerPath returns where the maiden-run marker lives: on the workspace volume, so
// it survives a volumes-only resume. For a Git repo it sits in the Git directory, which
// `git add -A` and `git clean -fdx` never touch. Otherwise it sits in .ax.
func workspaceMarkerPath(targetPath string) string {
	if info, err := os.Lstat(filepath.Join(targetPath, ".git")); err == nil && info.IsDir() {
		return filepath.Join(targetPath, ".git", markerDirName, InitializedMarkerFilename)
	}
	return filepath.Join(targetPath, markerDirName, InitializedMarkerFilename)
}

// writeMarker records a completed maiden run.
func writeMarker(path string, ws *v1alpha1.Workspace) error {
	name := "unknown"
	if ws != nil && ws.GetMetadata() != nil && ws.GetMetadata().GetName() != "" {
		name = ws.GetMetadata().GetName()
	}
	content := fmt.Sprintf("workspace: %s\ninitialized_at: %s\n", name, time.Now().UTC().Format(time.RFC3339))
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), filePerm)
}

// writeStateFile writes a diagnostic file under AXDir, logging rather than failing on error.
func writeStateFile(name string, data []byte) {
	path := filepath.Join(AXDir, name)
	if err := os.WriteFile(path, data, filePerm); err != nil {
		slog.Warn("failed to write state file", "path", path, "error", err)
	}
}

// writeFiles writes inlined files into the workspace directory.
func writeFiles(files []*v1alpha1.File, targetPath string) error {
	for _, f := range files {
		if f == nil || strings.TrimSpace(f.GetPath()) == "" {
			continue
		}
		rel := f.GetPath()
		if strings.HasSuffix(rel, "/") || filepath.Clean(rel) == "." {
			return fmt.Errorf("workspace file path %q must name a file, not a directory", rel)
		}
		if !isRelativeWithinRoot(rel) {
			return fmt.Errorf("workspace file path %q must be relative to workspace %s", rel, targetPath)
		}
		dest := filepath.Join(targetPath, rel)
		if err := os.MkdirAll(filepath.Dir(dest), dirPerm); err != nil {
			return fmt.Errorf("creating directory for workspace file %s: %w", dest, err)
		}
		if err := os.WriteFile(dest, []byte(f.GetContent()), filePerm); err != nil {
			return fmt.Errorf("writing workspace file %s: %w", dest, err)
		}
		slog.Info("wrote inlined workspace file", "path", dest)
	}
	return nil
}

func isRelativeWithinRoot(p string) bool {
	if filepath.IsAbs(p) {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	return clean != ".." && !strings.HasPrefix(clean, "../")
}

// markerLinked reports whether the marker's directory or the marker itself is a symbolic
// link. The agent can create one, and reading or writing through it would leave the workspace.
func markerLinked(path string) bool {
	for _, p := range []string{filepath.Dir(path), path} {
		if info, err := os.Lstat(p); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

// legacyOwnedBy reports whether a legacy marker was written for this workspace. Two paths can
// share a legacy name (/a/b and /a-b), so the recorded name is checked too.
func legacyOwnedBy(data []byte, ws *v1alpha1.Workspace) bool {
	name := ws.GetMetadata().GetName()
	if name == "" {
		name = "unknown"
	}
	return strings.HasPrefix(string(data), "workspace: "+name+"\n")
}
