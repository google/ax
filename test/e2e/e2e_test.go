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
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"go.ate.dev/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultAtespace = "default"

	phaseSuspended = "Suspended"
	phaseRunning   = "Running"

	// sampleRepo is a bare Git repository baked into the E2E task-runner image
	// (see test/e2e/Dockerfile.task-runner.e2e). Tag v1.0.0 contains
	// app.py printing "hello from git repo v1".
	sampleRepo = "file:///opt/ax-e2e-sample-repo.git"

	// probeURL is the in-sandbox address of the ax-task-runner probe server.
	probeURL = "http://127.0.0.1:80"

	// metadataURL is the in-sandbox address of the ax-task-runner metadata server.
	metadataURL = "http://127.0.0.1:8099"
)

// TestE2E_CLIVersionAndAutoTunnel verifies `ax version` and that, without
// --server, the CLI automatically opens a port-forward tunnel to ax-server
// that `ax ctx` reports as healthy and `ax tunnel stop` tears down.
func TestE2E_CLIVersionAndAutoTunnel(t *testing.T) {
	assertContains(t, "'ax version'", env.mustRunAX(t, "", "version"), "ax version")

	// Without --server, ax should auto-create a port-forward tunnel to
	// ax-server using the kube context. Use a dedicated AX_HOME so the tunnel
	// state is isolated from the rest of the suite.
	axHome := t.TempDir()
	runTunneled := func(args ...string) string {
		t.Helper()
		cmd, cancel := env.axCommand(axHome, append([]string{"--context", env.KubeContext, "-n", env.AXSystemNamespace}, args...)...)
		defer cancel()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("ax %s failed: %v\nOutput:\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	t.Cleanup(func() {
		cmd, cancel := env.axCommand(axHome, "tunnel", "stop", env.KubeContext)
		defer cancel()
		_ = cmd.Run()
	})

	runTunneled("get", "tasks")

	listOut := runTunneled("ctx")
	assertContains(t, "'ax ctx'", strings.ToLower(listOut), strings.ToLower(env.KubeContext), "healthy")

	runTunneled("tunnel", "stop", env.KubeContext)
	assertContains(t, "'ax ctx' after stop", runTunneled("ctx"), "No background tunnel running")
}

const workspaceCRUDManifest = `apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: {{.Name}}
  atespace: {{.Atespace}}
spec:
  git:
  - name: origin
    repo: {{.Repo}}
    branch: v1.0.0
  files:
  - path: config/settings.json
    content: '{"env":"{{.Env}}"}'
  mcp:
    servers:
    - name: local-mcp
      endpoint: http://localhost:9000
  skills:
    path: /workspace/.agents/skills
`

// TestE2E_WorkspaceCRUD verifies the Workspace lifecycle through the CLI:
// `ax apply` reports created / unchanged / configured, `ax get` and
// `ax describe` show the updated spec, and `ax delete` removes it.
func TestE2E_WorkspaceCRUD(t *testing.T) {
	ns := defaultAtespace
	name := uniqueName("ws-crud")
	env.deleteOnCleanup(t, ns, "workspace", name)

	manifest := func(envValue string) string {
		return renderYAML(t, workspaceCRUDManifest, map[string]string{
			"Name": name, "Atespace": ns, "Repo": sampleRepo, "Env": envValue,
		})
	}
	ref := "workspace.ax.io/" + name

	assertContains(t, "first apply", env.apply(t, ns, manifest("e2e-v1")), ref+" created")
	assertContains(t, "identical re-apply", env.apply(t, ns, manifest("e2e-v1")), ref+" unchanged")
	assertContains(t, "modified apply", env.apply(t, ns, manifest("e2e-v2")), ref+" configured")

	assertContains(t, "'ax get workspaces'", env.mustRunAX(t, ns, "get", "workspaces"), name)
	assertContains(t, "'ax get workspace'", env.mustRunAX(t, ns, "get", "workspace", name),
		"e2e-v2", "local-mcp", "config/settings.json")
	assertContains(t, "'ax describe workspace'", env.mustRunAX(t, ns, "describe", "workspace", name),
		"local-mcp", ".agents/skills")

	assertContains(t, "'ax delete workspace'", env.mustRunAX(t, ns, "delete", "workspace", name), "deleted")
	_, err := env.AXClient.GetWorkspace(t.Context(), &v1alpha1.GetWorkspaceRequest{Name: name, Atespace: ns})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("GetWorkspace after delete: got %v, want NotFound", err)
	}
}

const modelCRUDManifest = `apiVersion: ax.io/v1alpha1
kind: Model
metadata:
  name: {{.Name}}
  atespace: {{.Atespace}}
spec:
  provider: google
  model: {{.Model}}
  secretKey:
    name: gemini-secret
    key: api-key
`

// TestE2E_ModelCRUD verifies the same apply / get / describe / delete
// lifecycle as TestE2E_WorkspaceCRUD for a Model.
func TestE2E_ModelCRUD(t *testing.T) {
	ns := defaultAtespace
	name := uniqueName("model-crud")
	env.deleteOnCleanup(t, ns, "model", name)

	manifest := func(model string) string {
		return renderYAML(t, modelCRUDManifest, map[string]string{"Name": name, "Atespace": ns, "Model": model})
	}
	ref := "model.ax.io/" + name

	assertContains(t, "first apply", env.apply(t, ns, manifest("gemini-2.5-pro")), ref+" created")
	assertContains(t, "identical re-apply", env.apply(t, ns, manifest("gemini-2.5-pro")), ref+" unchanged")
	assertContains(t, "modified apply", env.apply(t, ns, manifest("gemini-2.5-flash")), ref+" configured")

	assertContains(t, "'ax get models'", env.mustRunAX(t, ns, "get", "models"), name, "gemini-2.5-flash")
	assertContains(t, "'ax get model'", env.mustRunAX(t, ns, "get", "model", name), "gemini-2.5-flash")
	assertContains(t, "'ax describe model'", env.mustRunAX(t, ns, "describe", "model", name), "gemini-secret")

	env.mustRunAX(t, ns, "delete", "model", name)
	_, err := env.AXClient.GetModel(t.Context(), &v1alpha1.GetModelRequest{Name: name, Atespace: ns})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("GetModel after delete: got %v, want NotFound", err)
	}
}

const debugTaskManifest = `apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: {{.Name}}
  atespace: {{.Atespace}}
spec:
  debug: {{.Debug}}
`

// taskManifest renders a minimal Task with no workspaces.
func taskManifest(t *testing.T, atespace, name string, debug bool) string {
	return renderYAML(t, debugTaskManifest, map[string]any{"Name": name, "Atespace": atespace, "Debug": debug})
}

// TestE2E_TaskLifecycleAndSubstrateSync verifies that a Task and its Substrate
// resources stay in sync across its lifecycle:
//   - A new Task starts Suspended and is immutable.
//   - Substrate has a suspended Actor backed by a per-task ActorTemplate
//     (<task>-tmpl-<hash>) with the /readyz:80 wakeup probe and a snapshot config.
//   - Resuming moves the Task to Running with WorkspaceReady and Ready
//     conditions, on the worker Substrate reports.
//   - Deleting the Task removes the Actor and ActorTemplate.
func TestE2E_TaskLifecycleAndSubstrateSync(t *testing.T) {
	ctx := t.Context()
	ns := defaultAtespace
	name := uniqueName("task-life")
	env.deleteOnCleanup(t, ns, "task", name)

	manifest := taskManifest(t, ns, name, true)

	// 1. A new task is created Suspended, backed by an Actor of the same name.
	assertContains(t, "'ax apply'", env.apply(t, ns, manifest), "task.ax.io/"+name+" created")
	task := env.getTask(t, ns, name)
	assertPhase(t, task, phaseSuspended)
	if got := task.GetStatus().GetActor(); got != name {
		t.Fatalf("task actor = %q, want %q", got, name)
	}

	// 2. Tasks are immutable: both CreateTask and `ax apply` reject a duplicate.
	_, err := env.AXClient.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: name, Atespace: ns},
		Spec:     &v1alpha1.TaskSpec{},
	}})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("duplicate CreateTask: got %v, want FailedPrecondition", err)
	}
	out, err := env.runAX(ns, "apply", "-f", writeManifest(t, manifest))
	if err == nil {
		t.Fatalf("expected re-applying an existing task to fail, got:\n%s", out)
	}
	assertContains(t, "duplicate 'ax apply'", out, "already exists and is immutable")

	// 3. Substrate has a suspended Actor using a per-task ActorTemplate.
	actor := env.getActor(t, ns, name, ateapipb.ActorState_ACTOR_STATE_SUSPENDED)
	tmplName := actor.GetActorTemplate().GetName()
	if !strings.HasPrefix(tmplName, name+"-tmpl-") {
		t.Fatalf("actor template = %q, want prefix %q", tmplName, name+"-tmpl-")
	}
	tmpl, err := env.SubstrateClient.GetActorTemplate(ctx, ns, tmplName)
	if err != nil {
		t.Fatalf("Substrate GetActorTemplate(%s/%s): %v", ns, tmplName, err)
	}
	if containers := tmpl.GetContainers(); len(containers) != 1 {
		t.Fatalf("actor template has %d containers, want 1", len(containers))
	}
	if probe := tmpl.GetContainers()[0].GetWakeupProbe().GetHttpGet(); probe.GetPath() != "/readyz" || probe.GetPort() != 80 {
		t.Fatalf("unexpected WakeupProbe: %+v", tmpl.GetContainers()[0].GetWakeupProbe())
	}
	// Newer Substrate versions may return SnapshotConfig fields AX's proto does
	// not know about, so accept either a known storage location or unknown fields.
	if sc := tmpl.GetSnapshotConfig(); sc.GetStorageLocation() == "" && len(sc.ProtoReflect().GetUnknown()) == 0 {
		t.Fatalf("unexpected SnapshotConfig: %+v", sc)
	}

	// 4. Resuming moves the task to Running on a worker reported by Substrate.
	assertContains(t, "'ax resume task'", env.mustRunAX(t, ns, "resume", "task", name), "resumed")
	task = env.getTask(t, ns, name)
	assertPhase(t, task, phaseRunning)
	assertCondition(t, task, "WorkspaceReady", "True", v1alpha1.ReasonSetupComplete)
	assertCondition(t, task, "Ready", "True", v1alpha1.ReasonTaskRunning)
	workerIP := task.GetStatus().GetWorkerIp()
	if workerIP == "" {
		t.Fatal("expected non-empty WorkerIp on running task")
	}
	actor = env.getActor(t, ns, name, ateapipb.ActorState_ACTOR_STATE_RUNNING)
	if ips := actor.GetStatus().GetWorkerAssignment().GetWorkerPodIps(); len(ips) == 0 || ips[0] != workerIP {
		t.Fatalf("Substrate WorkerPodIps = %v, want first IP %q", ips, workerIP)
	}

	// 5. Deleting the task removes the AX task, the Actor, and its ActorTemplate.
	assertContains(t, "'ax delete task'", env.mustRunAX(t, ns, "delete", "task", name), "deleted")
	if _, err := env.AXClient.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: name, Atespace: ns}); status.Code(err) != codes.NotFound {
		t.Fatalf("AX GetTask after delete: got %v, want NotFound", err)
	}
	if _, err := env.SubstrateClient.GetActor(ctx, ns, name); status.Code(err) != codes.NotFound {
		t.Fatalf("Substrate GetActor after delete: got %v, want NotFound", err)
	}
	if _, err := env.SubstrateClient.GetActorTemplate(ctx, ns, tmplName); status.Code(err) != codes.NotFound {
		t.Fatalf("Substrate GetActorTemplate(%s) after delete: got %v, want NotFound", tmplName, err)
	}
}

const metadataManifest = `apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: {{.Workspace}}
  atespace: {{.Atespace}}
spec:
  files:
  - path: hello.txt
    content: "hello from workspace"
---
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: {{.Task}}
  atespace: {{.Atespace}}
spec:
  debug: true
  workspace:
    name: {{.Workspace}}
    path: /workspace
  env:
  - name: CUSTOM_E2E_VAR
    value: "custom-val-123"
  command:
  - sh
  - -c
  - "echo cmd-ran-ok > /workspace/cmd-ran.txt"
`

// TestE2E_MetadataServerAndGuestExec verifies the guest environment of a
// running task: `ax ssh` sees spec.env, AX_METADATA_URL, and workspace files;
// spec.command runs; the in-sandbox metadata server serves health and
// task/workspace metadata; guest exit codes propagate; and `ax ssh` is
// rejected when spec.debug is false.
func TestE2E_MetadataServerAndGuestExec(t *testing.T) {
	ns := defaultAtespace
	ws := uniqueName("ws-meta")
	task := uniqueName("task-meta")
	env.deleteOnCleanup(t, ns, "workspace", ws)
	env.deleteOnCleanup(t, ns, "task", task)

	env.apply(t, ns, renderYAML(t, metadataManifest, map[string]string{"Workspace": ws, "Task": task, "Atespace": ns}))
	env.mustRunAX(t, ns, "resume", "task", task)

	// 1. `ax ssh` sees spec.env, the metadata URL, and workspace files.
	out := env.ssh(t, ns, task, "echo ENV=$CUSTOM_E2E_VAR META=$AX_METADATA_URL FILE=$(cat /workspace/hello.txt)")
	assertContains(t, "'ax ssh' output", out,
		"ENV=custom-val-123", "META="+metadataURL, "FILE=hello from workspace")

	// spec.command runs in the background after setup; wait for its output.
	err := poll(10*time.Second, 250*time.Millisecond, func() error {
		out, err := env.runAX(ns, "ssh", task, "--", "cat", "/workspace/cmd-ran.txt")
		if err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		if got := strings.TrimSpace(out); got != "cmd-ran-ok" {
			return fmt.Errorf("cmd-ran.txt = %q", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("spec.command did not write /workspace/cmd-ran.txt: %v", err)
	}

	// 2. The in-sandbox probe server serves health/readiness on port 80, and
	// the loopback metadata server serves task/workspace metadata at AX_METADATA_URL.
	curl := func(url string) string {
		t.Helper()
		return env.mustRunAX(t, ns, "ssh", task, "--", "curl", "-sf", url)
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		if got := strings.TrimSpace(curl(probeURL + path)); got != "ok" {
			t.Fatalf("GET %s = %q, want \"ok\"", path, got)
		}
	}
	assertContains(t, "task metadata", curl(metadataURL+"/metadata/v1alpha1/ax/task"), task, "custom-val-123")
	assertContains(t, "workspace metadata", curl(metadataURL+"/metadata/v1alpha1/ax/workspace"), ws, "hello from workspace")

	// 3. `ax ssh` propagates a non-zero guest exit code.
	if _, err := env.runAX(ns, "ssh", task, "--", "sh", "-c", "exit 17"); err == nil {
		t.Fatal("expected 'ax ssh' to fail when the guest command exits 17")
	}

	// 4. `ax ssh` is rejected for tasks without spec.debug.
	noDebug := uniqueName("task-nodebug")
	env.deleteOnCleanup(t, ns, "task", noDebug)
	env.apply(t, ns, taskManifest(t, ns, noDebug, false))
	env.mustRunAX(t, ns, "resume", "task", noDebug)
	out, err = env.runAX(ns, "ssh", noDebug, "--", "echo", "should-fail")
	if err == nil {
		t.Fatalf("expected 'ax ssh' to fail when debug=false, got:\n%s", out)
	}
	if !strings.Contains(out, "spec.debug: true") && !strings.Contains(out, "debugging is not enabled") {
		t.Fatalf("expected debug-disabled error, got:\n%s", out)
	}
}

const workspaceSetupManifest = `apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: {{.GitWorkspace}}
  atespace: {{.Atespace}}
spec:
  git:
  - name: sample
    dir: "."
    repo: {{.Repo}}
    branch: v1.0.0
  files:
  - path: overlay/config.yaml
    content: "mode: git-plus-overlay"
  skills:
    path: /workspace/primary/.agents/skills
---
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: {{.Task}}
  atespace: {{.Atespace}}
spec:
  debug: true
  workspace:
    name: {{.GitWorkspace}}
    path: /workspace/primary
`

// TestE2E_WorkspaceSetup_FilesAndGit verifies workspace setup for a task:
// custom mount path (/workspace/primary), working directory set to the mount
// path, spec.git checkout at a tag, overlay spec.files, spec.skills.path, and
// the /ax/initialized-* maiden-run marker.
func TestE2E_WorkspaceSetup_FilesAndGit(t *testing.T) {
	ns := defaultAtespace
	wsGit := uniqueName("ws-git")
	task := uniqueName("task-ws")
	env.deleteOnCleanup(t, ns, "workspace", wsGit)
	env.deleteOnCleanup(t, ns, "task", task)

	env.apply(t, ns, renderYAML(t, workspaceSetupManifest, map[string]string{
		"GitWorkspace": wsGit, "Task": task, "Atespace": ns, "Repo": sampleRepo,
	}))
	env.mustRunAX(t, ns, "resume", "task", task)

	// - The working directory is the workspace's mount path.
	// - The workspace has the repo checked out at v1.0.0, the overlay file, and
	//   the skills directory.
	// - A maiden-run marker is written for the workspace mount path.
	out := env.ssh(t, ns, task, `set -e
pwd
cat /workspace/primary/app.py
cat /workspace/primary/overlay/config.yaml
test -d /workspace/primary/.agents/skills && echo SKILLS_DIR_OK
test -f /ax/initialized-workspace-primary && echo MARKERS_OK
`)
	assertContains(t, "workspace setup check", out,
		"/workspace/primary",
		"hello from git repo v1",
		"mode: git-plus-overlay",
		"SKILLS_DIR_OK",
		"MARKERS_OK",
	)
}

const persistenceManifest = `apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: {{.Workspace}}
  atespace: {{.Atespace}}
spec:
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
  workspace:
    name: {{.Workspace}}
    path: /workspace
`

// TestE2E_TaskSuspendAndResumePersistence verifies that suspend/resume with
// VOLUMES-fidelity snapshots preserves the durable /workspace volume while
// the rootfs (/tmp) is reset.
func TestE2E_TaskSuspendAndResumePersistence(t *testing.T) {
	ns := defaultAtespace
	ws := uniqueName("ws-persist")
	task := uniqueName("task-persist")
	env.deleteOnCleanup(t, ns, "workspace", ws)
	env.deleteOnCleanup(t, ns, "task", task)

	env.apply(t, ns, renderYAML(t, persistenceManifest, map[string]string{"Workspace": ws, "Task": task, "Atespace": ns}))
	env.mustRunAX(t, ns, "resume", "task", task)

	// 1. Write to the durable /workspace volume and to the ephemeral rootfs.
	env.ssh(t, ns, task, "echo durable-data-42 > /workspace/durable.txt && echo ephemeral-tmp > /tmp/ephemeral.txt")

	// 2. Suspend: the task and its Actor are suspended and the worker released.
	assertContains(t, "'ax suspend task'", env.mustRunAX(t, ns, "suspend", "task", task), "suspended")
	got := env.getTask(t, ns, task)
	assertPhase(t, got, phaseSuspended)
	assertCondition(t, got, "Ready", "False", v1alpha1.PhaseSuspended)
	if ip := got.GetStatus().GetWorkerIp(); ip != "" {
		t.Fatalf("expected empty WorkerIp when suspended, got %q", ip)
	}
	actor := env.getActor(t, ns, task, ateapipb.ActorState_ACTOR_STATE_SUSPENDED)
	if len(actor.GetStatus().GetSnapshots()) == 0 && len(actor.GetStatus().ProtoReflect().GetUnknown()) == 0 {
		t.Fatal("expected non-empty Snapshots on suspended actor")
	}

	// 3. Resume: VOLUMES-fidelity snapshots restore /workspace but cold-boot the rootfs.
	env.mustRunAX(t, ns, "resume", "task", task)
	out := env.ssh(t, ns, task,
		"echo DURABLE=$(cat /workspace/durable.txt); if [ -f /tmp/ephemeral.txt ]; then echo TMP_EXISTS; else echo TMP_GONE; fi")
	assertContains(t, "post-resume check", out, "DURABLE=durable-data-42", "TMP_GONE")
}

// TestE2E_DescribeAndAtespaceIsolation verifies that a task in a custom
// atespace is invisible from the default atespace and that
// `ax describe task` reports the task once it is resumed.
func TestE2E_DescribeAndAtespaceIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	ns := uniqueName("team-e2e")
	task := uniqueName("task-describe")
	env.deleteOnCleanup(t, ns, "task", task)

	env.apply(t, ns, taskManifest(t, ns, task, true))

	// 1. The task is visible only in its own atespace.
	if _, err := env.AXClient.GetTask(ctx, &v1alpha1.GetTaskRequest{Name: task, Atespace: defaultAtespace}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetTask in %q atespace: got %v, want NotFound", defaultAtespace, err)
	}
	if out := env.mustRunAX(t, defaultAtespace, "get", "tasks"); strings.Contains(out, task) {
		t.Fatalf("task %s unexpectedly listed in %q atespace:\n%s", task, defaultAtespace, out)
	}
	assertContains(t, "'ax get tasks' in "+ns, env.mustRunAX(t, ns, "get", "tasks"), task)

	// 2. Resume the task.
	env.mustRunAX(t, ns, "resume", "task", task)

	// 3. `ax describe task` reflects the running task and its conditions.
	assertContains(t, "'ax describe task'", env.mustRunAX(t, ns, "describe", "task", task),
		task, ns, phaseRunning, "WorkspaceReady", v1alpha1.ReasonTaskRunning)
}
