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

package e2e_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

// statusManifest is a Task whose command runs Script, standing in for the
// agent that reports how the task ended in $AX_STATUS_FILE.
const statusManifest = `apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: {{.Workspace}}
  atespace: {{.Atespace}}
spec:
  profile:
    image: {{taskImage}}
---
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: {{.Task}}
  atespace: {{.Atespace}}
spec:
  profile:
    image: {{taskImage}}
  debug: true
  workspace:
    name: {{.Workspace}}
    path: /workspace
  commands:
  - sh
  - -c
  - |
{{.Script}}
`

// report is a script that writes status to $AX_STATUS_FILE atomically, as an
// agent should. The runner does not make the file's directory.
func report(status string) string {
	return fmt.Sprintf(`    set -e
    mkdir -p "$(dirname "$AX_STATUS_FILE")"
    echo '%s' > "$AX_STATUS_FILE.tmp"
    mv "$AX_STATUS_FILE.tmp" "$AX_STATUS_FILE"`, status)
}

// TestE2E_TaskStatusFile verifies that ax-server records how a task ended from
// the status file its agent writes:
//   - COMPLETED and FAILED become the task's state, with the Ready reason,
//     the message, and status.outcome.output.
//   - The state survives suspend and resume.
//   - A command that exits without a status file leaves the task running:
//     the exit status does not say how the task ended.
func TestE2E_TaskStatusFile(t *testing.T) {
	ns := defaultAtespace
	tasks := []struct {
		name, script string
		want         v1alpha1.TaskState // UNSPECIFIED: still running
		reason       string
		message      string
		output       string
	}{
		{
			name:   uniqueName("task-completed"),
			script: report(`{"state": "COMPLETED", "message": "all done", "output": "the answer is 42"}`),
			want:   v1alpha1.TaskState_TASK_STATE_COMPLETED, reason: v1alpha1.ReasonTaskCompleted,
			message: "all done", output: "the answer is 42",
		},
		{
			name:   uniqueName("task-failed"),
			script: report(`{"state": "FAILED", "message": "tests failed: 3 of 120"}`),
			want:   v1alpha1.TaskState_TASK_STATE_FAILED, reason: v1alpha1.ReasonTaskFailed,
			message: "tests failed: 3 of 120",
		},
		{
			name:   uniqueName("task-exits"),
			script: "    exit 3",
		},
	}
	for _, tc := range tasks {
		ws := uniqueName("ws-status")
		env.deleteOnCleanup(t, ns, "workspace", ws)
		env.deleteOnCleanup(t, ns, "task", tc.name)
		env.apply(t, ns, renderYAML(t, statusManifest, map[string]string{
			"Workspace": ws, "Task": tc.name, "Atespace": ns, "Script": tc.script,
		}))
		env.mustRunAX(t, ns, "resume", "task", tc.name)
	}

	// 1. ax-server asks every running task's runner every few seconds and
	// records the first COMPLETED or FAILED.
	for _, tc := range tasks {
		if tc.want == v1alpha1.TaskState_TASK_STATE_UNSPECIFIED {
			continue
		}
		var task *v1alpha1.Task
		err := poll(2*time.Minute, time.Second, func() error {
			task = env.getTask(t, ns, tc.name)
			if got := task.GetStatus().GetState(); got != tc.want {
				return fmt.Errorf("state = %v, want %v", got, tc.want)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("task %s: %v", tc.name, err)
		}
		assertCondition(t, task, "Ready", "False", tc.reason)
		if got := task.GetStatus().GetOutcome().GetOutput(); got != tc.output {
			t.Errorf("task %s: outcome.output = %q, want %q", tc.name, got, tc.output)
		}
		assertContains(t, "'ax describe task "+tc.name+"'", env.mustRunAX(t, ns, "describe", "task", tc.name),
			tc.reason, tc.message, tc.output)
	}

	// 2. After a few more passes of ax-server's loop, the task whose command
	// exited without a status file is still running.
	time.Sleep(15 * time.Second)
	exits := tasks[2].name
	got := env.getTask(t, ns, exits)
	assertState(t, got, v1alpha1.TaskState_TASK_STATE_RUNNING)
	if got.GetStatus().GetOutcome() != nil {
		t.Errorf("task %s: outcome = %v, want none", exits, got.GetStatus().GetOutcome())
	}

	// 3. A terminal state survives suspend and resume.
	completed := tasks[0]
	env.mustRunAX(t, ns, "suspend", "task", completed.name)
	assertState(t, env.getTask(t, ns, completed.name), completed.want)
	env.mustRunAX(t, ns, "resume", "task", completed.name)
	got = env.getTask(t, ns, completed.name)
	assertState(t, got, completed.want)
	if out := got.GetStatus().GetOutcome().GetOutput(); out != completed.output {
		t.Errorf("task %s after resume: outcome.output = %q, want %q", completed.name, out, completed.output)
	}
}
