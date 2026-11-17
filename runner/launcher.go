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

package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/encoding/protojson"
)

// The launcher contract. The runner hands a task to the program that runs its
// agent as files, never as container environment, so the ActorTemplate holds
// no per-task data. Each variable below names one of the files and is set in
// the environment of the task's command. When spec.commands is empty and the
// task has a goal, the runner starts the image's launcher at
// DefaultLauncherPath to consume them.
const (
	// GoalFileEnv names the file holding spec.goal.text.
	GoalFileEnv = "AX_GOAL_FILE"
	// ConfigFileEnv names the file holding spec.config as JSON.
	ConfigFileEnv = "AX_CONFIG_FILE"
	// MCPConfigFileEnv names the file holding the Workspace's MCP servers in
	// the common {"mcpServers": {...}} JSON format.
	MCPConfigFileEnv = "AX_MCP_CONFIG_FILE"
	// SessionFileEnv names the file holding spec.session_id.
	SessionFileEnv = "AX_SESSION_FILE"
	// OutputFileEnv names the file the launcher writes the agent's answer to.
	// It is on the workspace volume so it survives suspend, and AX reports it
	// as status.outcome.output.
	OutputFileEnv = "AX_OUTPUT_FILE"

	// DefaultLauncherPath is where the runner looks for the image's launcher
	// when Config.LauncherPath is empty.
	DefaultLauncherPath = "/usr/local/bin/ax-harness"
	// DefaultRunDir is where the goal, config, MCP and session files are
	// written when Config.RunDir is empty. It is rewritten on every start, so
	// a sandbox restored from a snapshot never sees a stale goal.
	DefaultRunDir = "/run/ax"

	// launcherDoneFile, under the workspace's .ax directory, records that the
	// launcher finished, so a resumed sandbox does not run a one-shot task
	// again.
	launcherDoneFile = "launcher-done"
)

// launcherFiles are the files written for a task and their digest.
type launcherFiles struct {
	// env points the task's command at the files.
	env []string
	// digest identifies the goal, config, MCP servers and session, so that a
	// finished launcher is not started again for the same input.
	digest string
}

// writeLauncherFiles writes the task's goal, config, the Workspace's MCP
// servers and the session ID under runDir, and makes the directory of the
// output file under wsPath.
func writeLauncherFiles(runDir, wsPath string, task *v1alpha1.Task, ws *v1alpha1.Workspace) (launcherFiles, error) {
	spec := task.GetSpec()
	config := []byte("{}")
	if c := spec.GetConfig(); c != nil {
		b, err := protojson.Marshal(c)
		if err != nil {
			return launcherFiles{}, fmt.Errorf("encoding spec.config: %w", err)
		}
		config = b
	}
	mcp, err := json.Marshal(map[string]any{"mcpServers": MCPServers(ws.GetSpec().GetMcp())})
	if err != nil {
		return launcherFiles{}, fmt.Errorf("encoding workspace MCP servers: %w", err)
	}
	files := []struct {
		env, name string
		data      []byte
	}{
		{GoalFileEnv, "goal.md", []byte(spec.GetGoal().GetText())},
		{ConfigFileEnv, "config.json", config},
		{MCPConfigFileEnv, "mcp.json", mcp},
		{SessionFileEnv, "session-id", []byte(spec.GetSessionId())},
	}

	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return launcherFiles{}, fmt.Errorf("creating %s: %w", runDir, err)
	}
	var out launcherFiles
	h := sha256.New()
	for _, f := range files {
		p := filepath.Join(runDir, f.name)
		if err := os.WriteFile(p, f.data, 0o644); err != nil {
			return launcherFiles{}, fmt.Errorf("writing %s: %w", p, err)
		}
		out.env = append(out.env, f.env+"="+p)
		_, _ = fmt.Fprintf(h, "%s=%d:%s;", f.name, len(f.data), f.data)
	}
	output := outputPath(wsPath)
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return launcherFiles{}, fmt.Errorf("creating %s: %w", filepath.Dir(output), err)
	}
	out.env = append(out.env, OutputFileEnv+"="+output)
	out.digest = hex.EncodeToString(h.Sum(nil))
	return out, nil
}

// outputPath is where the launcher writes the agent's answer for a workspace.
func outputPath(wsPath string) string {
	return filepath.Join(wsPath, ".ax", "output")
}

// launcherCommand returns the command that consumes a task without
// spec.commands: the launcher, unless the task has no goal (so users can
// attach to the sandbox instead), the image has no launcher, or the launcher
// already finished for the same input before a suspend. It returns nil when
// nothing should run.
func launcherCommand(launcher, wsPath string, task *v1alpha1.Task, files launcherFiles) []string {
	if task.GetSpec().GetGoal().GetText() == "" {
		slog.Info("no task command or goal; serving metadata until stopped")
		return nil
	}
	if _, err := os.Stat(launcher); err != nil {
		slog.Info("no task command and no launcher in the image; serving metadata until stopped", "launcher", launcher)
		return nil
	}
	if done, err := os.ReadFile(doneMarkerPath(wsPath)); err == nil && string(done) == files.digest {
		slog.Info("launcher already finished for this goal; not starting it again", "launcher", launcher)
		return nil
	}
	return []string{launcher}
}

// markLauncherDone records that the launcher finished for files' input.
func markLauncherDone(wsPath string, files launcherFiles) {
	p := doneMarkerPath(wsPath)
	if err := os.WriteFile(p, []byte(files.digest), 0o644); err != nil {
		slog.Warn("failed to record that the launcher finished", "path", p, "error", err)
	}
}

func doneMarkerPath(wsPath string) string {
	return filepath.Join(wsPath, ".ax", launcherDoneFile)
}

// MCPServers converts a Workspace's MCP servers, both the static ones and
// those listed under its registries, into the mcpServers object of the common
// MCP config format that Claude Code, Antigravity and other agents read. A
// server with an endpoint is reached over streamable HTTP; one with a command
// is started over stdio. Servers with neither, or with a name already taken,
// are skipped.
func MCPServers(mcp *v1alpha1.MCPConfig) map[string]any {
	all := slices.Clone(mcp.GetServers())
	for _, r := range mcp.GetRegistries() {
		all = append(all, r.GetServers()...)
	}
	servers := map[string]any{}
	for _, srv := range all {
		name := srv.GetName()
		if _, dup := servers[name]; dup || name == "" {
			slog.Warn("skipping MCP server with a missing or duplicate name", "name", name)
			continue
		}
		switch {
		case srv.GetEndpoint() != "":
			servers[name] = map[string]any{"type": "http", "url": srv.GetEndpoint()}
		case srv.GetCommand() != "":
			servers[name] = map[string]any{"type": "stdio", "command": srv.GetCommand(), "args": srv.GetArgs()}
		default:
			slog.Warn("skipping MCP server with neither an endpoint nor a command", "name", name)
		}
	}
	return servers
}
