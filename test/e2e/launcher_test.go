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
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

const launcherTaskManifest = `apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: {{.Task}}
  atespace: {{.Atespace}}
spec:
  profile:
    image: {{.Image}}
  goal:
    text: {{.Goal}}
  sessionId: {{.Session}}
  debug: true
`

// TestE2E_PureImageLauncher runs a task on an image that has no AX runner in
// it, only a launcher, the way the prebuilt agent images are built. It checks
// that the runner comes from the mounted runner volume, that the goal and
// session reach the launcher as files, that the launcher's answer lands in
// $AX_OUTPUT_FILE on the workspace, and that suspend and resume keep the
// answer without running the launcher again.
func TestE2E_PureImageLauncher(t *testing.T) {
	image := os.Getenv("AX_E2E_LAUNCHER_IMAGE")
	if image == "" {
		t.Skip("AX_E2E_LAUNCHER_IMAGE is not set; run ./hack/setup-e2e-kind.sh")
	}
	ns := defaultAtespace
	task := uniqueName("task-launcher")
	env.deleteOnCleanup(t, ns, "task", task)

	const goal, session = "summarize-the-repo", "session-e2e"
	env.apply(t, ns, renderYAML(t, launcherTaskManifest, map[string]string{
		"Task": task, "Atespace": ns, "Image": image, "Goal": goal, "Session": session,
	}))
	env.mustRunAX(t, ns, "resume", "task", task)

	// 1. The image is pure: the runner runs from the read-only runner volume.
	out := env.ssh(t, ns, task, fmt.Sprintf(`test ! -e /usr/local/bin/ax-task-runner && echo NO_BAKED_RUNNER
test -x %s/ax-task-runner && echo RUNNER_MOUNTED
touch %s/x 2>/dev/null || echo RUNNER_READ_ONLY`, v1alpha1.RunnerMountPath, v1alpha1.RunnerMountPath))
	assertContains(t, "runner check", out, "NO_BAKED_RUNNER", "RUNNER_MOUNTED", "RUNNER_READ_ONLY")

	// 2. The launcher ran with the goal and session as files and wrote its
	// answer to $AX_OUTPUT_FILE.
	wantOutput := []string{"goal=" + goal, "session=" + session}
	waitForOutput(t, ns, task, wantOutput)

	// 3. Suspend and resume keep the answer on the workspace volume, and the
	// launcher does not run again for the same goal.
	env.mustRunAX(t, ns, "suspend", "task", task)
	assertState(t, env.getTask(t, ns, task), v1alpha1.TaskState_TASK_STATE_SUSPENDED)
	env.mustRunAX(t, ns, "resume", "task", task)
	waitForOutput(t, ns, task, wantOutput)
	if runs := strings.TrimSpace(env.ssh(t, ns, task, "grep -c run /workspace/.ax/e2e-launcher-runs")); runs != "1" {
		t.Fatalf("launcher ran %s times across a suspend and resume, want 1", runs)
	}
}

// waitForOutput polls the task's $AX_OUTPUT_FILE until it contains every want.
func waitForOutput(t *testing.T, ns, task string, wants []string) {
	t.Helper()
	err := poll(time.Minute, time.Second, func() error {
		out, err := env.runAX(ns, "ssh", task, "--", "cat", "/workspace/.ax/output")
		if err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		for _, want := range wants {
			if !strings.Contains(out, want) {
				return fmt.Errorf("output = %q, want %q", out, want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("launcher output: %v", err)
	}
}
