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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

// getStatus reads the runner's status endpoint.
func getStatus(t *testing.T, port int) v1alpha1.RunnerStatus {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, v1alpha1.RunnerStatusPath))
	if err != nil {
		t.Fatalf("GET %s: %v", v1alpha1.RunnerStatusPath, err)
	}
	defer resp.Body.Close()
	var st v1alpha1.RunnerStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, %v", v1alpha1.RunnerStatusPath, resp.StatusCode, err)
	}
	return st
}

func TestRun_ServesHowTheTaskEnded(t *testing.T) {
	const completed, failed = v1alpha1.RunnerStateCompleted, v1alpha1.RunnerStateFailed
	for _, tc := range []struct {
		name, script string
		want         v1alpha1.RunnerStatus // a Message matches as a substring
	}{
		{"exit zero without a status file", `exit 0`, v1alpha1.RunnerStatus{}},
		{"exit non-zero without a status file", `exit 3`, v1alpha1.RunnerStatus{}},
		{"status file", `echo '{"state": "FAILED", "message": "tests failed: 3 of 120"}' > "$AX_STATUS_FILE"`, v1alpha1.RunnerStatus{State: failed, Message: "tests failed: 3 of 120"}},
		{"status file with output", `echo '{"state": "COMPLETED", "output": "the answer"}' > "$AX_STATUS_FILE"`, v1alpha1.RunnerStatus{State: completed, Output: "the answer"}},
		{"status file wins over the exit status", `echo '{"state": "COMPLETED"}' > "$AX_STATUS_FILE"; exit 1`, v1alpha1.RunnerStatus{State: completed}},
		{"not JSON", `echo done > "$AX_STATUS_FILE"`, v1alpha1.RunnerStatus{State: failed, Message: "parsing status file"}},
		{"unknown state", `echo '{"state": "DONE"}' > "$AX_STATUS_FILE"`, v1alpha1.RunnerStatus{State: failed, Message: `unknown state "DONE"`}},
		{"over 16 KiB", `printf '{"state": "COMPLETED", "message": "%s"}' "$(head -c 17000 /dev/zero | tr '\0' x)" > "$AX_STATUS_FILE"`, v1alpha1.RunnerStatus{State: failed, Message: "exceeds 16384 bytes"}},
		{"REQUIRES_ACTION is not acted on", `echo '{"state": "REQUIRES_ACTION"}' > "$AX_STATUS_FILE"`, v1alpha1.RunnerStatus{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			// The runner does not make the status file's directory.
			h.task.Spec.Commands = []string{"sh", "-c", `mkdir -p "$(dirname "$AX_STATUS_FILE")"; ` + tc.script}
			h.runUntilCommandExits(t)

			got := getStatus(t, h.cfg.Port)
			if tc.want.Message != "" && strings.Contains(got.Message, tc.want.Message) {
				got.Message = tc.want.Message
			}
			if got != tc.want {
				t.Errorf("status = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The status file is served as soon as it is there, so a long-lived command
// can report how the task ended before it exits.
func TestRun_ServesStatusWhileCommandRuns(t *testing.T) {
	h := newHarness(t)
	report := filepath.Join(t.TempDir(), "report")
	h.task.Spec.Commands = []string{"sh", "-c", `while [ ! -f ` + report + ` ]; do sleep 0.02; done
mkdir -p "$(dirname "$AX_STATUS_FILE")"
echo '{"state": "COMPLETED"}' > "$AX_STATUS_FILE.tmp" && mv "$AX_STATUS_FILE.tmp" "$AX_STATUS_FILE"
sleep 30`}
	h.start(t)
	waitHealthy(t, h.cfg.Port)

	if got := getStatus(t, h.cfg.Port); got != (v1alpha1.RunnerStatus{}) {
		t.Errorf("status = %+v before the command reports, want none", got)
	}
	if err := os.WriteFile(report, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); getStatus(t, h.cfg.Port).State != v1alpha1.RunnerStateCompleted; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the running command's status was never served")
		}
	}
}

// The runner never writes the status file: the exit status is not a report.
func TestRun_DoesNotWriteStatusFileFromExitStatus(t *testing.T) {
	h := newHarness(t)
	h.task.Spec.Commands = []string{"sh", "-c", "exit 3"}
	h.runUntilCommandExits(t)

	if _, err := os.Stat(filepath.Join(h.wsPath, ".ax", "status")); !os.IsNotExist(err) {
		t.Errorf("status file after exit 3: %v, want none", err)
	}
}

// The status file is on the workspace volume, so a sandbox resumed after the
// task ended still reports how.
func TestRun_ServesStatusFileFromBeforeSuspend(t *testing.T) {
	h := newHarness(t)
	if err := os.MkdirAll(filepath.Join(h.wsPath, ".ax"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.wsPath, ".ax", "status"), []byte(`{"state": "FAILED", "message": "boom"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	h.start(t)
	if code := waitWorkspaceCheck(t, h.cfg.Port); code != http.StatusOK {
		t.Fatalf("workspace check = %d, want 200", code)
	}

	if got, want := getStatus(t, h.cfg.Port), (v1alpha1.RunnerStatus{State: v1alpha1.RunnerStateFailed, Message: "boom"}); got != want {
		t.Errorf("status = %+v, want %+v", got, want)
	}
}
