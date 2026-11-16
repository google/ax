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

package runner_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/google/ax/runner"
	"google.golang.org/protobuf/types/known/structpb"
)

// launcherScript is a stand-in for an image's ax-harness: it copies every
// launcher contract file into the output file, one per line, and counts its
// runs in runs.
const launcherScript = `#!/bin/sh
echo run >> "$(dirname "$0")/runs"
{
  cat "$AX_GOAL_FILE"; echo
  cat "$AX_CONFIG_FILE"; echo
  cat "$AX_MCP_CONFIG_FILE"; echo
  cat "$AX_SESSION_FILE"; echo
} > "$AX_OUTPUT_FILE"
`

// withLauncher points h at a fake launcher and a temp run dir, gives the task
// a goal, and returns the launcher's directory.
func withLauncher(t *testing.T, h *harness, goal string) string {
	t.Helper()
	dir := t.TempDir()
	launcher := filepath.Join(dir, "ax-harness")
	if err := os.WriteFile(launcher, []byte(launcherScript), 0o755); err != nil {
		t.Fatal(err)
	}
	h.cfg.LauncherPath = launcher
	h.cfg.RunDir = filepath.Join(dir, "run")
	h.task.Spec.Goal = &v1alpha1.Goal{Text: goal}
	return dir
}

func runs(t *testing.T, launcherDir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(launcherDir, "runs"))
	if os.IsNotExist(err) {
		return 0
	} else if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "run")
}

func TestRun_StartsLauncherWithContractFiles(t *testing.T) {
	h := newHarness(t)
	withLauncher(t, h, "Summarize the repo.")
	config, err := structpb.NewStruct(map[string]any{"model": "fast"})
	if err != nil {
		t.Fatal(err)
	}
	h.task.Spec.Config = config
	h.task.Spec.SessionId = "s-1"
	h.cfg.Workspace = &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "ws"},
		Spec: &v1alpha1.WorkspaceSpec{Mcp: &v1alpha1.MCPConfig{
			Servers: []*v1alpha1.MCPServer{{Name: "docs", Endpoint: "https://example.com/mcp"}},
		}},
	}

	if exit := h.runUntilCommandExits(t); exit.Err != nil {
		t.Fatalf("unexpected exit: %+v", exit)
	}
	out, err := os.ReadFile(filepath.Join(h.wsPath, ".ax", "output"))
	if err != nil {
		t.Fatalf("launcher did not write $AX_OUTPUT_FILE under the workspace: %v", err)
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) < 4 {
		t.Fatalf("output = %q, want goal, config, MCP config and session", out)
	}
	if lines[0] != "Summarize the repo." {
		t.Errorf("goal file = %q", lines[0])
	}
	var gotConfig map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &gotConfig); err != nil || gotConfig["model"] != "fast" {
		t.Errorf("config file = %q (%v), want spec.config", lines[1], err)
	}
	var gotMCP struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &gotMCP); err != nil || gotMCP.MCPServers["docs"]["url"] != "https://example.com/mcp" {
		t.Errorf("MCP config file = %q (%v), want the workspace's servers", lines[2], err)
	}
	if lines[3] != "s-1" {
		t.Errorf("session file = %q, want s-1", lines[3])
	}
}

// Without a goal the launcher is not started, so the sandbox stays up for
// users to attach to.
func TestRun_NoGoalDoesNotStartLauncher(t *testing.T) {
	h := newHarness(t)
	dir := withLauncher(t, h, "")
	h.start(t)

	waitHealthy(t, h.cfg.Port)
	time.Sleep(200 * time.Millisecond)
	if n := runs(t, dir); n != 0 {
		t.Errorf("launcher ran %d times without a goal, want 0", n)
	}
}

// A sandbox resumed after the launcher finished does not run the same goal
// again, but does run a new one.
func TestRun_DoesNotRestartFinishedLauncher(t *testing.T) {
	h := newHarness(t)
	dir := withLauncher(t, h, "first goal")
	h.runUntilCommandExits(t)
	h.cancel()
	h.waitFinished(t, 5*time.Second)

	again := newHarness(t)
	again.wsPath, again.task.Spec.Workspace.Path = h.wsPath, h.wsPath
	again.cfg.LauncherPath, again.cfg.RunDir = h.cfg.LauncherPath, h.cfg.RunDir
	again.task.Spec.Goal = &v1alpha1.Goal{Text: "first goal"}
	again.start(t)
	waitHealthy(t, again.cfg.Port)
	time.Sleep(200 * time.Millisecond)
	if n := runs(t, dir); n != 1 {
		t.Fatalf("launcher ran %d times for the same goal, want 1", n)
	}
	again.cancel()
	again.waitFinished(t, 5*time.Second)

	next := newHarness(t)
	next.wsPath, next.task.Spec.Workspace.Path = h.wsPath, h.wsPath
	next.cfg.LauncherPath, next.cfg.RunDir = h.cfg.LauncherPath, h.cfg.RunDir
	next.task.Spec.Goal = &v1alpha1.Goal{Text: "second goal"}
	next.runUntilCommandExits(t)
	if n := runs(t, dir); n != 2 {
		t.Errorf("launcher ran %d times after a new goal, want 2", n)
	}
	if out, _ := os.ReadFile(filepath.Join(h.wsPath, ".ax", "output")); !strings.HasPrefix(string(out), "second goal") {
		t.Errorf("output = %q, want the second goal's", out)
	}
}

// Explicit commands also get the contract's variables, so any command can
// write $AX_OUTPUT_FILE; the launcher is not started for them.
func TestRun_CommandsGetContractEnv(t *testing.T) {
	h := newHarness(t)
	dir := withLauncher(t, h, "goal")
	h.task.Spec.Commands = []string{"sh", "-c", `cat "$AX_GOAL_FILE" > "$AX_OUTPUT_FILE"`}

	if exit := h.runUntilCommandExits(t); exit.Err != nil {
		t.Fatalf("unexpected exit: %+v", exit)
	}
	if out, _ := os.ReadFile(filepath.Join(h.wsPath, ".ax", "output")); string(out) != "goal" {
		t.Errorf("output = %q, want the goal", out)
	}
	if n := runs(t, dir); n != 0 {
		t.Errorf("launcher ran %d times for a task with commands, want 0", n)
	}
}

func TestRun_MissingLauncherServes(t *testing.T) {
	h := newHarness(t)
	withLauncher(t, h, "goal")
	h.cfg.LauncherPath = filepath.Join(t.TempDir(), "missing")
	h.start(t)

	waitHealthy(t, h.cfg.Port)
	select {
	case <-h.finished:
		t.Fatalf("Run returned before cancellation: %v", h.err)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestMCPServers(t *testing.T) {
	got := runner.MCPServers(&v1alpha1.MCPConfig{
		Servers: []*v1alpha1.MCPServer{
			{Name: "docs", Endpoint: "https://example.com/mcp"},
			{Name: "git", Command: "git-mcp", Args: []string{"--repo", "."}},
			{Name: "empty"},
		},
		Registries: []*v1alpha1.MCPRegistry{{Servers: []*v1alpha1.MCPServer{
			{Name: "docs", Endpoint: "https://shadowed.example.com/mcp"},
		}}},
	})
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"docs":{"type":"http","url":"https://example.com/mcp"},"git":{"args":["--repo","."],"command":"git-mcp","type":"stdio"}}`
	if string(b) != want {
		t.Errorf("MCPServers = %s, want %s", b, want)
	}
}
