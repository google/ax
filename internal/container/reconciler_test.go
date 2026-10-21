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

package container

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeCLI is a shell script standing in for podman. It keeps the container's
// state in a file, logs every invocation, and records whether the secret
// environment reached it.
const fakeCLI = `#!/bin/sh
D="$FAKE_DIR"
echo "$@" >> "$D/calls"
case "$1" in
  inspect) if [ -f "$D/state" ]; then cat "$D/state"; else echo "Error: no such container $6" >&2; exit 125; fi ;;
  image) [ -f "$D/no-image" ] && { echo "Error: image not known" >&2; exit 125; }; echo sha256:abc ;;
  pull) rm -f "$D/no-image"; echo "pulled $2" ;;
  create) echo created > "$D/state"; printf '%s' "$AX_TASK_YAML" > "$D/task-yaml"; printf '%s' "$GEMINI_API_KEY" > "$D/gemini" ;;
  start|unpause) [ -f "$D/fail-start" ] && { echo "Error: crun: executable file not found" >&2; exit 126; }; echo running > "$D/state" ;;
  pause) echo paused > "$D/state" ;;
  port) cat "$D/port" ;;
  rm) rm -f "$D/state" ;;
  volume) echo removed ;;
  logs) echo "runner log line" ;;
esac
`

type fakeRuntime struct {
	dir string
	rt  *Runtime
}

func newFakeRuntime(t *testing.T, readyAddr string) *fakeRuntime {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "podman")
	if err := os.WriteFile(bin, []byte(fakeCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "port"), []byte(readyAddr+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_DIR", dir)
	return &fakeRuntime{dir: dir, rt: &Runtime{Bin: bin}}
}

func (f *fakeRuntime) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(f.dir, name))
	return strings.TrimSpace(string(b))
}

func (f *fakeRuntime) touch(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readyServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func condition(task *v1alpha1.Task, typ string) *v1alpha1.Condition {
	for _, c := range task.GetStatus().GetConditions() {
		if c.Type == typ {
			return c
		}
	}
	return nil
}

func newTask() *v1alpha1.Task {
	return &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "t1"},
		Spec: &v1alpha1.TaskSpec{
			Image: "example.com/runner:1",
			Env:   []*v1alpha1.EnvVar{{Name: "GREETING", Value: "hi"}},
			Resources: &v1alpha1.ResourcesSpec{
				Limits: &v1alpha1.ResourceList{Cpu: "500m", Memory: "1Gi"},
			},
		},
		Status: &v1alpha1.TaskStatus{Phase: "Suspended"},
	}
}

func TestReconcileLifecycle(t *testing.T) {
	ctx := context.Background()
	addr := readyServer(t)
	f := newFakeRuntime(t, addr)
	f.touch(t, "no-image")
	t.Setenv("GEMINI_API_KEY", "sekret")
	r := NewReconciler(Options{Runtime: f.rt, WorkspaceReadyTimeout: 2 * time.Second})

	// Create: pulls the image and creates a stopped container.
	task, err := r.Reconcile(ctx, newTask(), &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "ws"}})
	if err != nil {
		t.Fatalf("Reconcile(create): %v", err)
	}
	if task.Status.Phase != "Suspended" || task.Status.Actor != "t1" || task.Status.WorkerIp != "" {
		t.Errorf("after create status = %v", task.Status)
	}
	if f.read("state") != StateCreated {
		t.Errorf("container state = %q, want created", f.read("state"))
	}
	calls := f.read("calls")
	for _, want := range []string{
		"pull example.com/runner:1",
		"create --name ax.default.t1 --entrypoint " + GuestCommand,
		"--label ax.io/task=t1",
		"--publish 127.0.0.1::80",
		"--volume ax.default.t1.workspace:/workspace",
		"--cpus 0.500",
		"--memory 1073741824b",
		"--env GREETING=hi",
		"--env AX_TASK_YAML",
		"--env AX_WORKSPACE_YAML",
		"--env GEMINI_API_KEY",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("CLI calls missing %q:\n%s", want, calls)
		}
	}
	if strings.Contains(calls, "sekret") {
		t.Errorf("secret leaked onto the command line:\n%s", calls)
	}
	if f.read("gemini") != "sekret" {
		t.Errorf("GEMINI_API_KEY not passed through the environment")
	}
	if y := f.read("task-yaml"); !strings.Contains(y, "name: t1") || strings.Contains(y, "phase") {
		t.Errorf("AX_TASK_YAML = %q, want launch config without status", y)
	}

	// Resume: starts the container and waits for the workspace.
	task.Status.Phase = "Running"
	task, err = r.Reconcile(ctx, task, nil)
	if err != nil {
		t.Fatalf("Reconcile(resume): %v", err)
	}
	if task.Status.Phase != "Running" || task.Status.WorkerIp != addr {
		t.Errorf("after resume status = %v", task.Status)
	}
	if c := condition(task, condReady); c == nil || c.Status != "True" {
		t.Errorf("Ready = %v, want True", c)
	}
	if c := condition(task, condWorkspaceReady); c == nil || c.Status != "True" {
		t.Errorf("WorkspaceReady = %v, want True", c)
	}

	// Suspend pauses; resume unpauses.
	task.Status.Phase = "Suspended"
	if task, err = r.Reconcile(ctx, task, nil); err != nil || f.read("state") != StatePaused {
		t.Fatalf("suspend: err=%v state=%q", err, f.read("state"))
	}
	if c := condition(task, condReady); c.Reason != "TaskSuspended" {
		t.Errorf("Ready reason after suspend = %q", c.Reason)
	}
	task.Status.Phase = "Running"
	if _, err = r.Reconcile(ctx, task, nil); err != nil || f.read("state") != StateRunning {
		t.Fatalf("resume from pause: err=%v state=%q", err, f.read("state"))
	}
	if !strings.Contains(f.read("calls"), "unpause ax.default.t1") {
		t.Errorf("resume from pause did not unpause")
	}

	// Delete removes the container and volume; deleting again is fine.
	if err := r.ReconcileDelete(ctx, "default", "t1"); err != nil {
		t.Fatalf("ReconcileDelete: %v", err)
	}
	if f.read("state") != "" || !strings.Contains(f.read("calls"), "volume rm --force ax.default.t1.workspace") {
		t.Errorf("delete left state %q; calls:\n%s", f.read("state"), f.read("calls"))
	}
}

func TestReconcileStartFailure(t *testing.T) {
	f := newFakeRuntime(t, readyServer(t))
	f.touch(t, "fail-start")
	r := NewReconciler(Options{Runtime: f.rt})
	task := newTask()
	task.Status.Phase = "Running"
	task, err := r.Reconcile(context.Background(), task, nil)
	if err == nil {
		t.Fatal("Reconcile succeeded, want start failure")
	}
	if task.Status.Phase != "Failed" {
		t.Errorf("phase = %q, want Failed", task.Status.Phase)
	}
	if c := condition(task, condReady); c == nil || c.Reason != "ContainerStartFailed" || !strings.Contains(c.Message, "executable file not found") {
		t.Errorf("Ready = %v, want ContainerStartFailed with the runtime's message", c)
	}
}

func TestReconcileRejectsInvalidResources(t *testing.T) {
	r := NewReconciler(Options{Runtime: &Runtime{Bin: "/nonexistent"}})
	task := newTask()
	task.Spec.Resources.Limits.Cpu = "lots"
	task, err := r.Reconcile(context.Background(), task, nil)
	if err == nil || task.Status.Phase != "Failed" {
		t.Fatalf("Reconcile = %v, phase %q; want Failed", err, task.Status.Phase)
	}
}

func TestWrapErr(t *testing.T) {
	r := &Runtime{Bin: "/usr/bin/podman"}
	if err := r.wrapErr([]string{"inspect"}, "Error: no such container x", errors.New("exit 125")); !errors.Is(err, ErrNotFound) {
		t.Errorf("no such container: %v, want ErrNotFound", err)
	}
	err := r.wrapErr([]string{"ps"}, "Cannot connect to Podman. Please verify your connection", errors.New("exit 125"))
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "podman machine start") {
		t.Errorf("unreachable engine: %v, want Unavailable with a hint", err)
	}
}

func TestLimits(t *testing.T) {
	for _, tt := range []struct {
		cpu, mem string
		wantCPU  string
		wantMem  int64
	}{
		{"", "", "", 0},
		{"2", "512Mi", "2.000", 512 << 20},
		{"250m", "1G", "0.250", 1_000_000_000},
	} {
		cpus, mem, err := limits(&v1alpha1.ResourcesSpec{Limits: &v1alpha1.ResourceList{Cpu: tt.cpu, Memory: tt.mem}})
		if err != nil || cpus != tt.wantCPU || mem != tt.wantMem {
			t.Errorf("limits(%q, %q) = %q, %d, %v; want %q, %d", tt.cpu, tt.mem, cpus, mem, err, tt.wantCPU, tt.wantMem)
		}
	}
}
