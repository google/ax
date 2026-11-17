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
	"testing"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

const promoteManifest = `apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: {{.Workspace}}
  atespace: {{.Atespace}}
spec:
  setup:
    files:
    - path: seed.txt
      content: "initial-seed"
---
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: {{.Task}}
  atespace: {{.Atespace}}
spec:
  debug: true
  gateway:
    name: {{.Gateway}}
  workspace:
    name: {{.Workspace}}
`

// TestE2E_Promote verifies that `ax promote` copies a running task, with the
// Gateway and Workspace it binds, to another cluster and runs it there:
//   - --suspend-source suspends the source task.
//   - The promoted task is a new task with the same session ID, in a
//     workspace set up from the Workspace.
//   - --replace recreates a task that the target already has.
func TestE2E_Promote(t *testing.T) {
	remote := env.remote(t)
	ns := defaultAtespace
	gw, ws, task := uniqueName("gw-promote"), uniqueName("ws-promote"), uniqueName("task-promote")
	for _, cluster := range []*suiteEnv{env, remote} {
		cluster.deleteOnCleanup(t, ns, "gateway", gw)
		cluster.deleteOnCleanup(t, ns, "workspace", ws)
		cluster.deleteOnCleanup(t, ns, "task", task)
	}
	// --to names the remote cluster by its kube context, so the CLI opens a
	// tunnel to it.
	t.Cleanup(func() { _, _ = env.runAX("", "tunnel", "stop", remote.KubeContext) })

	// Setup: a running task on the source cluster.
	env.apply(t, ns, renderYAML(t, gatewayCRUDManifest, map[string]string{
		"Name": gw, "Atespace": ns, "Host": "ax-echo." + env.AXSystemNamespace + ".svc",
	}))
	env.apply(t, ns, renderYAML(t, promoteManifest, map[string]string{
		"Gateway": gw, "Workspace": ws, "Task": task, "Atespace": ns,
	}))
	env.mustRunAX(t, ns, "resume", "task", task)
	promote := func(flags ...string) string {
		t.Helper()
		args := []string{"promote", "task", task, "--to", remote.KubeContext, "--to-namespace", remote.AXSystemNamespace}
		return env.mustRunAX(t, ns, append(args, flags...)...)
	}

	// 1. The task is suspended on the source.
	assertContains(t, "'ax promote'", promote("--suspend-source"),
		"gateway.ax.io/"+gw+" created", "workspace.ax.io/"+ws+" created", "task.ax.io/"+task+" suspended on the source")
	source := env.getTask(t, ns, task)
	assertState(t, source, v1alpha1.TaskState_TASK_STATE_SUSPENDED)

	// 2. The promoted task is a new task with the same session ID, and runs
	// in a workspace set up from the Workspace.
	promoted := remote.getTask(t, ns, task)
	if promoted.GetMetadata().GetUid() == source.GetMetadata().GetUid() {
		t.Fatalf("promoted task has the source's UID %q", source.GetMetadata().GetUid())
	}
	if got, want := promoted.GetSpec().GetSessionId(), source.GetSpec().GetSessionId(); got != want {
		t.Fatalf("promoted task: sessionId = %q, want the source's %q", got, want)
	}
	assertContains(t, "promoted workspace", remote.ssh(t, ns, task, "cat /workspace/seed.txt"), "initial-seed")

	// 3. --replace recreates the task that the target now has, behind the
	// Gateway and in the Workspace that are already there.
	assertContains(t, "'ax promote --replace'", promote("--replace"),
		"gateway.ax.io/"+gw+" reused", "workspace.ax.io/"+ws+" unchanged", "task.ax.io/"+task+" replaced")
	replaced := remote.getTask(t, ns, task)
	assertState(t, replaced, v1alpha1.TaskState_TASK_STATE_WORKING)
	if replaced.GetMetadata().GetUid() == promoted.GetMetadata().GetUid() {
		t.Fatalf("task after --replace still has UID %q", promoted.GetMetadata().GetUid())
	}
}
