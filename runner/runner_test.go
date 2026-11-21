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
	"encoding/json"
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

// Run links git-askpass to the running program, here the test binary. Package
// runner answers under that name before main starts; if it ever does not,
// fail git's prompt instead of running the tests again from inside a test.
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "git-askpass" {
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func TestRun_StartsTaskCommandWithInjectedEnv(t *testing.T) {
	h := newHarness(t)
	outFile := filepath.Join(t.TempDir(), "out.txt")
	h.task.Spec.Env = []*v1alpha1.EnvVar{{Name: "DEMO_ENV", Value: "from-task"}}
	h.task.Spec.Commands = []string{"sh", "-c", `echo "$DEMO_ENV $AX_METADATA_URL $(pwd)" > ` + outFile}

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
	h.task.Spec.Commands = []string{"sh", "-c", "exit 3"}

	exit := h.runUntilCommandExits(t)
	if exit.ExitCode != 3 || exit.Err == nil {
		t.Errorf("expected exit code 3 with an error, got %+v", exit)
	}
}

func TestRun_KeepsServingAfterCommandExits(t *testing.T) {
	h := newHarness(t)
	h.task.Spec.Commands = []string{"true"}

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
	h.task.Spec.Commands = []string{"sh", "-c", "sleep 30; sleep 30"}
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

// The runner points the CA variables at the bundle when it exists and has git
// ask the runner for credentials, which are a placeholder, except where the
// task sets the variable.
func TestRun_SetsDefaultEnv(t *testing.T) {
	bundle := filepath.Join(t.TempDir(), "trust-bundle.pem")
	if err := os.WriteFile(bundle, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		caBundle string
		env      []*v1alpha1.EnvVar
		want     string // SSL_CERT_FILE, GIT_SSL_CAINFO, GIT_TERMINAL_PROMPT, git's password
	}{
		{name: "defaults", caBundle: bundle, want: bundle + "|" + bundle + "|0|password=ax-placeholder"},
		{name: "bundle missing", caBundle: bundle + ".missing", want: "||0|password=ax-placeholder"},
		{
			name:     "set by the task",
			caBundle: bundle,
			env: []*v1alpha1.EnvVar{
				{Name: "SSL_CERT_FILE", Value: "/task/ca.pem"},
				{Name: "GIT_ASKPASS", Value: "true"}, // answers with nothing
			},
			want: "/task/ca.pem|" + bundle + "|0|password=",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range []string{
				"SSL_CERT_FILE", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS", "GIT_SSL_CAINFO",
				"GIT_ASKPASS", "GIT_TERMINAL_PROMPT",
			} {
				t.Setenv(name, "") // restores the variable when the test ends
				os.Unsetenv(name)
			}
			h := newHarness(t)
			// A link left by an earlier run is replaced.
			if err := os.MkdirAll(workspace.AXDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("/stale", filepath.Join(workspace.AXDir, "git-askpass")); err != nil {
				t.Fatal(err)
			}
			outFile := filepath.Join(t.TempDir(), "out.txt")
			h.cfg.CABundle = tc.caBundle
			h.task.Spec.Env = tc.env
			h.task.Spec.Commands = []string{"sh", "-c", `echo "$SSL_CERT_FILE|$GIT_SSL_CAINFO|$GIT_TERMINAL_PROMPT|$(printf 'protocol=https\nhost=git.example\n\n' | git credential fill 2>/dev/null | grep password)" > ` + outFile}

			if exit := h.runUntilCommandExits(t); exit.Err != nil {
				t.Fatalf("unexpected exit: %+v", exit)
			}
			out, err := os.ReadFile(outFile)
			if err != nil {
				t.Fatalf("command did not write its output: %v", err)
			}
			got := strings.TrimSpace(string(out))
			if got != tc.want {
				t.Errorf("command's environment = %q, want %q", got, tc.want)
			}
			// ax ssh sessions get the runner's own environment, which must agree.
			if caFile := strings.Split(tc.want, "|")[0]; os.Getenv("SSL_CERT_FILE") != caFile {
				t.Errorf("runner's SSL_CERT_FILE = %q, want %q", os.Getenv("SSL_CERT_FILE"), caFile)
			}
		})
	}
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

	wsPath := filepath.Join(dir, "workspace")
	origWSDir := runner.DefaultWorkspacePath
	runner.DefaultWorkspacePath = wsPath
	t.Cleanup(func() { runner.DefaultWorkspacePath = origWSDir })

	h := &harness{
		wsPath:   wsPath,
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
		RunDir:        filepath.Join(dir, "run"),
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
	h.task.Spec.Commands = []string{"sh", "-c", "pwd > " + outFile}

	exit := h.runUntilCommandExits(t)
	if exit.Err != nil || exit.ExitCode != 0 {
		t.Fatalf("unexpected exit: %+v", exit)
	}

	// The workspace directory exists and has its maiden-run marker.
	if st, err := os.Stat(codePath); err != nil || !st.IsDir() {
		t.Errorf("workspace dir %s missing: %v", codePath, err)
	}
	marker := filepath.Join(codePath, ".ax", workspace.InitializedMarkerFilename)
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

func TestRun_SetupFailureAbortsBeforeStartingCommand(t *testing.T) {
	h := newHarness(t)
	ranMarker := filepath.Join(t.TempDir(), "command-ran")

	h.cfg.Workspace = &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Setup: &v1alpha1.WorkspaceSetup{
				Commands: []string{"echo setup-boom >&2 && exit 9"},
			},
		},
	}
	h.task.Spec.Commands = []string{"sh", "-c", "touch " + ranMarker}

	err := runner.Run(t.Context(), h.cfg)
	if err == nil || !strings.Contains(err.Error(), "setup-boom") {
		t.Fatalf("Run() = %v, want setup error containing %q", err, "setup-boom")
	}
	if _, statErr := os.Stat(ranMarker); !os.IsNotExist(statErr) {
		t.Fatalf("spec.command started despite workspace setup failure")
	}
}

// pushYAML posts body to path on the runner's probe port and returns the status.
func pushYAML(t *testing.T, port int, path, body string) int {
	t.Helper()
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d%s", port, path), "application/yaml", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// waitWorkspaceCheck polls /readyz?check=workspace until it answers other than
// 503 and returns that status.
func waitWorkspaceCheck(t *testing.T, port int) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/readyz?check=workspace", port))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable {
				return resp.StatusCode
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("workspace check never settled")
	return 0
}

func newPushHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.cfg.Task = nil
	h.cfg.AwaitPush = true
	noDrop := false
	h.cfg.DropPrivileges = &noDrop
	return h
}

func TestRun_AwaitPushRunsPushedTask(t *testing.T) {
	h := newPushHarness(t)
	outFile := filepath.Join(t.TempDir(), "out.txt")
	h.cfg.PrepareTask = func(task *v1alpha1.Task, ws *v1alpha1.Workspace) (*v1alpha1.Task, error) {
		task.Spec.Env = append(task.Spec.Env, &v1alpha1.EnvVar{Name: "PREPARED", Value: ws.GetMetadata().GetName()})
		return task, nil
	}
	h.start(t)
	waitHealthy(t, h.cfg.Port)

	// Substrate's WakeupProbe reads plain /readyz, which is ready while the
	// runner waits for work.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/readyz", h.cfg.Port))
	if err != nil {
		t.Fatalf("readyz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz while waiting = %d, want 200", resp.StatusCode)
	}

	body := fmt.Sprintf(`metadata: {atespace: default, name: pushed, uid: u1}
spec:
  workspace: {name: code, path: %q}
  commands: [sh, -c, 'echo "$PREPARED" > %s']
---
metadata: {atespace: default, name: code}
spec:
  setup:
    files: [{path: hello.txt, content: hi}]
`, h.wsPath, outFile)
	if code := pushYAML(t, h.cfg.Port, v1alpha1.RunnerTaskPath, body); code != http.StatusAccepted {
		t.Fatalf("task push = %d, want 202", code)
	}
	if exit := h.waitExit(t); exit.Err != nil {
		t.Fatalf("pushed command failed: %+v", exit)
	}
	if out, _ := os.ReadFile(outFile); strings.TrimSpace(string(out)) != "code" {
		t.Errorf("command saw PREPARED=%q, want code", out)
	}
	if got, _ := os.ReadFile(filepath.Join(h.wsPath, "hello.txt")); string(got) != "hi" {
		t.Errorf("hello.txt = %q, want the pushed workspace's file", got)
	}
	if code := waitWorkspaceCheck(t, h.cfg.Port); code != http.StatusOK {
		t.Errorf("workspace check = %d, want 200", code)
	}
	// ax-server repeats the push on every resume.
	if code := pushYAML(t, h.cfg.Port, v1alpha1.RunnerTaskPath, body); code != http.StatusOK {
		t.Errorf("repeated task push = %d, want 200", code)
	}
}

func TestRun_AwaitPushSetsUpPushedWorkspace(t *testing.T) {
	h := newPushHarness(t)
	h.start(t)
	waitHealthy(t, h.cfg.Port)

	body := "metadata: {atespace: default, name: code, uid: w1}\nspec:\n  setup:\n    files: [{path: hello.txt, content: hi}]\n"
	if code := pushYAML(t, h.cfg.Port, v1alpha1.RunnerSetupPath, body); code != http.StatusAccepted {
		t.Fatalf("setup push = %d, want 202", code)
	}
	if code := waitWorkspaceCheck(t, h.cfg.Port); code != http.StatusOK {
		t.Fatalf("workspace check = %d, want 200", code)
	}
	if got, _ := os.ReadFile(filepath.Join(h.wsPath, "hello.txt")); string(got) != "hi" {
		t.Errorf("hello.txt = %q, want the pushed workspace's file", got)
	}
}

func TestRun_AwaitPushReportsSetupFailure(t *testing.T) {
	h := newPushHarness(t)
	h.start(t)
	waitHealthy(t, h.cfg.Port)

	body := "metadata: {atespace: default, name: code, uid: w1}\nspec:\n  setup:\n    commands: ['exit 7']\n"
	if code := pushYAML(t, h.cfg.Port, v1alpha1.RunnerSetupPath, body); code != http.StatusAccepted {
		t.Fatalf("setup push = %d, want 202", code)
	}
	if code := waitWorkspaceCheck(t, h.cfg.Port); code != http.StatusInternalServerError {
		t.Fatalf("workspace check = %d, want 500", code)
	}
}

// getStatus polls the runner's status endpoint until it reports how the task
// ended.
func getStatus(t *testing.T, port int) v1alpha1.RunnerStatus {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, v1alpha1.RunnerStatusPath)
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusOK:
				var st v1alpha1.RunnerStatus
				if err := json.Unmarshal(body, &st); err != nil {
					t.Fatalf("decoding %s reply %q: %v", v1alpha1.RunnerStatusPath, body, err)
				}
				return st
			case http.StatusNoContent:
			default:
				t.Fatalf("GET %s = %d %q, want 204 or 200", v1alpha1.RunnerStatusPath, resp.StatusCode, body)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no status reported on %s", v1alpha1.RunnerStatusPath)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The runner reports how the task ended: the state the command wrote to
// AX_STATUS_FILE, or the one its exit status implies, with AX_OUTPUT_FILE (or
// the end of stdout and stderr) as the output.
func TestRun_ReportsTaskStatus(t *testing.T) {
	cases := []struct {
		name        string
		script      string
		wantState   string
		wantMessage string
		wantCode    int32
		wantOutput  string // substring
	}{
		{
			name:      "exit zero completes",
			script:    `printf 'the answer' > "$AX_OUTPUT_FILE"`,
			wantState: v1alpha1.RunnerStateCompleted, wantOutput: "the answer",
		},
		{
			name:      "non-zero exit fails",
			script:    "echo from-stdout; echo from-stderr >&2; exit 3",
			wantState: v1alpha1.RunnerStateFailed, wantMessage: "exit status 3", wantCode: 3, wantOutput: "from-stderr",
		},
		{
			name:      "status file wins over exit status",
			script:    `printf '{"state":"FAILED","message":"tests failed: 3 of 120"}' > "$AX_STATUS_FILE"`,
			wantState: v1alpha1.RunnerStateFailed, wantMessage: "tests failed: 3 of 120",
		},
		{
			name:      "status file can complete a non-zero exit",
			script:    `printf '{"state":"COMPLETED","message":"partial is fine"}' > "$AX_STATUS_FILE"; exit 1`,
			wantState: v1alpha1.RunnerStateCompleted, wantMessage: "partial is fine", wantCode: 1,
		},
		{
			name:      "malformed status file fails",
			script:    `echo done > "$AX_STATUS_FILE"`,
			wantState: v1alpha1.RunnerStateFailed, wantMessage: "invalid AX_STATUS_FILE",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.task.Spec.Commands = []string{"sh", "-c", tc.script}
			h.runUntilCommandExits(t)
			got := getStatus(t, h.cfg.Port)
			if got.State != tc.wantState || !strings.Contains(got.Message, tc.wantMessage) || got.ExitCode != tc.wantCode || !strings.Contains(got.Output, tc.wantOutput) {
				t.Errorf("status = %+v, want state %s, message containing %q, exit code %d, output containing %q",
					got, tc.wantState, tc.wantMessage, tc.wantCode, tc.wantOutput)
			}
		})
	}
}

// A command that writes no status gets one from its exit status, written to
// AX_STATUS_FILE. A command that runs again after a resume, because ax-server
// had not recorded how it ended, does not inherit that status: it is cleared
// before the command starts, and the new run reports its own.
func TestRun_StatusOfEarlierRunIsCleared(t *testing.T) {
	h := newHarness(t)
	h.task.Spec.Commands = []string{"sh", "-c", "exit 4"}
	h.runUntilCommandExits(t)
	if got := getStatus(t, h.cfg.Port); got.State != v1alpha1.RunnerStateFailed || got.ExitCode != 4 {
		t.Fatalf("first run status = %+v, want FAILED with exit code 4", got)
	}
	raw, err := os.ReadFile(filepath.Join(h.wsPath, ".ax", "STATUS"))
	if err != nil || !strings.Contains(string(raw), `"state":"FAILED"`) {
		t.Fatalf("AX_STATUS_FILE = %q (%v), want the FAILED status written by the runner", raw, err)
	}
	h.cancel()
	h.waitFinished(t, 5*time.Second)

	again := newHarness(t)
	again.wsPath, again.task.Spec.Workspace.Path = h.wsPath, h.wsPath
	again.task.Spec.Commands = []string{"sh", "-c", `test ! -e "$AX_STATUS_FILE"`}
	again.cfg.RunDir = h.cfg.RunDir
	again.runUntilCommandExits(t)
	if got := getStatus(t, again.cfg.Port); got.State != v1alpha1.RunnerStateCompleted || got.ExitCode != 0 {
		t.Errorf("second run status = %+v, want COMPLETED: the first run's status must be gone when it starts", got)
	}
}

// A command that is still running has no status yet.
func TestRun_NoStatusWhileRunning(t *testing.T) {
	h := newHarness(t)
	h.task.Spec.Commands = []string{"sleep", "60"}
	h.start(t)
	waitHealthy(t, h.cfg.Port)
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", h.cfg.Port, v1alpha1.RunnerStatusPath))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("GET %s while the command runs = %d, want 204", v1alpha1.RunnerStatusPath, resp.StatusCode)
	}
}
