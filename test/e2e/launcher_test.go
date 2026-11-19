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
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
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

const pureImageIdleManifest = `apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: {{.Task}}
  atespace: {{.Atespace}}
spec:
  profile:
    image: {{.Image}}
  debug: true
`

// TestE2E_PureImageLauncher runs tasks on an image that has no AX runner in
// it, only a launcher, the way the prebuilt agent images are built. It checks
// that the runner comes from the mounted read-only runner volume, that the goal
// and session reach the launcher as files, and that when the launcher exits the
// task completes with the launcher's $AX_OUTPUT_FILE as its outcome, parks its
// actor, and cannot be resumed again.
func TestE2E_PureImageLauncher(t *testing.T) {
	image := os.Getenv("AX_E2E_LAUNCHER_IMAGE")
	if image == "" {
		t.Skip("AX_E2E_LAUNCHER_IMAGE is not set; run ./hack/setup-e2e-kind.sh")
	}
	ns := defaultAtespace

	// 1. An interactive task on the pure image stays Working so `ax ssh` can
	// verify the runner comes from the read-only runner volume mount.
	idleTask := uniqueName("task-pure-idle")
	env.deleteOnCleanup(t, ns, "task", idleTask)
	env.apply(t, ns, renderYAML(t, pureImageIdleManifest, map[string]string{
		"Task": idleTask, "Atespace": ns, "Image": image,
	}))
	env.mustRunAX(t, ns, "resume", "task", idleTask)

	out := env.ssh(t, ns, idleTask, fmt.Sprintf(`test ! -e /usr/local/bin/ax-task-runner && echo NO_BAKED_RUNNER
test -x %s/ax-task-runner && echo RUNNER_MOUNTED
touch %s/x 2>/dev/null || echo RUNNER_READ_ONLY`, v1alpha1.RunnerMountPath, v1alpha1.RunnerMountPath))
	assertContains(t, "runner check", out, "NO_BAKED_RUNNER", "RUNNER_MOUNTED", "RUNNER_READ_ONLY")

	// 2. A goal-driven task runs the image's launcher with goal and session
	// files, captures $AX_OUTPUT_FILE into status.outcome.output, completes the
	// task, and parks the actor.
	task := uniqueName("task-launcher")
	env.deleteOnCleanup(t, ns, "task", task)

	const goal, session = "summarize-the-repo", "session-e2e"
	env.apply(t, ns, renderYAML(t, launcherTaskManifest, map[string]string{
		"Task": task, "Atespace": ns, "Image": image, "Goal": goal, "Session": session,
	}))
	env.mustRunAX(t, ns, "resume", "task", task)

	var completed *v1alpha1.Task
	if err := poll(time.Minute, 500*time.Millisecond, func() error {
		got, err := env.AXClient.GetTask(t.Context(), &v1alpha1.GetTaskRequest{Name: task, Atespace: ns})
		if err != nil {
			return err
		}
		if got.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_COMPLETED {
			return fmt.Errorf("state = %v, want COMPLETED", got.GetStatus().GetState())
		}
		completed = got
		return nil
	}); err != nil {
		t.Fatalf("waiting for launcher task completion: %v", err)
	}

	outcome := completed.GetStatus().GetOutcome()
	if outcome.GetResult() != v1alpha1.TaskResult_TASK_RESULT_SUCCEEDED || outcome.GetExitCode() != 0 {
		t.Fatalf("unexpected outcome: %+v", outcome)
	}
	assertContains(t, "launcher outcome output", outcome.GetOutput(), "goal="+goal, "session="+session)

	if err := poll(time.Minute, 500*time.Millisecond, func() error {
		actor, err := env.SubstrateClient.GetActor(t.Context(), ns, task)
		if err != nil {
			return err
		}
		if got := actor.GetStatus().GetState(); got != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
			return fmt.Errorf("actor state = %v, want SUSPENDED", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("waiting for completed task actor to park: %v", err)
	}

	// 3. A completed task is terminal: resuming or suspending it is rejected so
	// the launcher cannot run again for the same goal.
	for _, op := range []string{"resume", "suspend"} {
		out, err := env.runAX(ns, op, "task", task)
		if err == nil {
			t.Fatalf("expected 'ax %s task %s' on completed task to fail, got:\n%s", op, task, out)
		}
		assertContains(t, op+" on completed task", strings.ToLower(out), "terminal")
	}
}
