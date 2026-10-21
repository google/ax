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
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/ax/internal/workspace"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/google/ax/runner"
)

func TestRun_StartsTaskCommandWithInjectedEnv(t *testing.T) {
	h := newHarness(t)
	outFile := filepath.Join(t.TempDir(), "out.txt")
	h.task.Spec.Env = []*v1alpha1.EnvVar{{Name: "DEMO_ENV", Value: "from-task"}}
	h.task.Spec.Command = []string{"sh", "-c", `echo "$DEMO_ENV $AX_METADATA_URL $(pwd)" > ` + outFile}

	exit := h.runUntilCommandExits(t)
	if exit.Err != nil || exit.ExitCode != 0 {
		t.Fatalf("unexpected exit: %+v", exit)
	}

	out, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("command did not write its output: %v", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 3 || fields[0] != "from-task" || fields[1] != fmt.Sprintf("http://127.0.0.1:%d", h.cfg.MetadataPort) {
		t.Errorf("unexpected env in command: %q", out)
	}
	// The working directory is the workspace path; compare resolved paths since
	// temp dirs may involve symlinks.
	wantDir, _ := filepath.EvalSymlinks(h.wsPath)
	if gotDir, _ := filepath.EvalSymlinks(fields[2]); gotDir != wantDir {
		t.Errorf("command cwd = %q, want workspace %q", fields[2], h.wsPath)
	}
}

func TestRun_ReportsCommandExitCode(t *testing.T) {
	h := newHarness(t)
	h.task.Spec.Command = []string{"sh", "-c", "exit 3"}

	exit := h.runUntilCommandExits(t)
	if exit.ExitCode != 3 || exit.Err == nil {
		t.Errorf("expected exit code 3 with an error, got %+v", exit)
	}
}

func TestRun_KeepsServingAfterCommandExits(t *testing.T) {
	h := newHarness(t)
	h.task.Spec.Command = []string{"true"}

	h.runUntilCommandExits(t)

	// The command is gone but the runner, and with it the metadata server, is not.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", h.cfg.Port))
	if err != nil {
		t.Fatalf("metadata server not reachable after command exit: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d, want 200", resp.StatusCode)
	}
	select {
	case <-h.finished:
		t.Fatalf("Run returned before cancellation: %v", h.err)
	default:
	}
}

func TestRun_StopsRunningCommandOnCancel(t *testing.T) {
	h := newHarness(t)
	// A shell with a sleeping child: the whole process group must be stopped.
	h.task.Spec.Command = []string{"sh", "-c", "sleep 30; sleep 30"}
	h.start(t)

	// Give the command a moment to start before cancelling.
	time.Sleep(100 * time.Millisecond)
	h.cancel()

	h.waitFinished(t, 5*time.Second)
	if exit := h.waitExit(t); exit.Err == nil {
		t.Errorf("expected the stopped command to report a signal exit, got %+v", exit)
	}
}

func TestRun_WithoutCommandWaitsForContext(t *testing.T) {
	h := newHarness(t)
	h.start(t)

	select {
	case <-h.finished:
		t.Fatalf("Run returned before cancellation: %v", h.err)
	case <-time.After(200 * time.Millisecond):
	}

	h.cancel()
	h.waitFinished(t, 5*time.Second)
}

func TestRun_DefaultsWithoutTask(t *testing.T) {
	// A nil Task still serves metadata; the port comes from Config.
	h := newHarness(t)
	h.cfg.Task = nil
	h.start(t)

	waitHealthy(t, h.cfg.Port)
	h.cancel()
	h.waitFinished(t, 5*time.Second)
}

// waitHealthy polls the metadata server's health endpoint until it answers.
func waitHealthy(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("metadata server never became reachable")
}

// harness runs the runner in the background with all state under a temp dir,
// a free port, and a hook that captures the command's exit.
type harness struct {
	cfg    runner.Config
	task   *v1alpha1.Task
	wsPath string

	cancel   context.CancelFunc
	finished chan struct{}
	err      error
	exited   chan runner.CommandExit
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()

	origAXDir := workspace.AXDir
	workspace.AXDir = filepath.Join(dir, "ax")
	t.Cleanup(func() { workspace.AXDir = origAXDir })

	h := &harness{
		wsPath:   filepath.Join(dir, "workspace"),
		finished: make(chan struct{}),
		exited:   make(chan runner.CommandExit, 1),
	}
	h.task = &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "lib-task", Atespace: "default"},
		Spec:     &v1alpha1.TaskSpec{Workspace: &v1alpha1.WorkspaceRef{Name: "ws", Path: h.wsPath}},
	}
	h.cfg = runner.Config{
		Port:          freePort(t),
		MetadataPort:  freePort(t),
		Task:          h.task,
		OnCommandExit: func(e runner.CommandExit) { h.exited <- e },
	}
	return h
}

// start launches Run in the background. Cleanup cancels it and waits for it to
// return so no runner outlives its test.
func (h *harness) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		h.err = runner.Run(ctx, h.cfg)
		close(h.finished)
	}()
	t.Cleanup(func() {
		cancel()
		h.waitFinished(t, 15*time.Second)
	})
}

// waitFinished blocks until Run has returned and fails the test if it returned
// an error or did not return in time. It is safe to call more than once.
func (h *harness) waitFinished(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-h.finished:
		if h.err != nil {
			t.Fatalf("Run returned error: %v", h.err)
		}
	case <-time.After(timeout):
		t.Fatal("Run did not return in time")
	}
}

func (h *harness) waitExit(t *testing.T) runner.CommandExit {
	t.Helper()
	select {
	case e := <-h.exited:
		return e
	case <-time.After(10 * time.Second):
		t.Fatal("command did not exit in time")
		return runner.CommandExit{}
	}
}

// runUntilCommandExits starts the runner and blocks until the command has
// exited, leaving the runner itself alive.
func (h *harness) runUntilCommandExits(t *testing.T) runner.CommandExit {
	t.Helper()
	h.start(t)
	return h.waitExit(t)
}

func freePort(t *testing.T) int {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	return lis.Addr().(*net.TCPAddr).Port
}

func TestRun_SetsUpBoundWorkspace(t *testing.T) {
	h := newHarness(t)
	root := t.TempDir()
	codePath := filepath.Join(root, "code")
	outFile := filepath.Join(root, "out.txt")

	h.task.Spec.Workspace = &v1alpha1.WorkspaceRef{Name: "code", Path: codePath}
	h.cfg.Workspace = &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "code"}, Spec: &v1alpha1.WorkspaceSpec{}}
	h.task.Spec.Command = []string{"sh", "-c", "pwd > " + outFile}

	exit := h.runUntilCommandExits(t)
	if exit.Err != nil || exit.ExitCode != 0 {
		t.Fatalf("unexpected exit: %+v", exit)
	}

	// The workspace directory exists and has its maiden-run marker.
	if st, err := os.Stat(codePath); err != nil || !st.IsDir() {
		t.Errorf("workspace dir %s missing: %v", codePath, err)
	}
	marker := filepath.Join(workspace.AXDir, workspace.MarkerName(codePath))
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("marker %s missing: %v", marker, err)
	}

	// The command ran in the workspace.
	out, _ := os.ReadFile(outFile)
	wantDir, _ := filepath.EvalSymlinks(codePath)
	if gotDir, _ := filepath.EvalSymlinks(strings.TrimSpace(string(out))); gotDir != wantDir {
		t.Errorf("command cwd = %q, want workspace %q", strings.TrimSpace(string(out)), codePath)
	}

	// Readiness reflects the workspace, and the metadata server serves it.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/readyz", h.cfg.Port))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("readyz = %d, want 200", resp.StatusCode)
	}
	resp, err = http.Get(fmt.Sprintf("http://127.0.0.1:%d/metadata/v1alpha1/ax/workspace", h.cfg.MetadataPort))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "name: code") {
		t.Errorf("workspace endpoint should serve the bound workspace:\n%s", body)
	}
}
