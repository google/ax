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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/ax/internal/runnerhttp"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/google/ax/runner"
)

type pullTestApplier struct {
	mu   sync.Mutex
	rows map[store.TaskKey]*store.TaskRow
}

func (a *pullTestApplier) GetTaskRow(_ context.Context, key store.TaskKey) (*store.TaskRow, error) {
	key = key.Normalized()
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.rows[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return r.Clone(), nil
}

func (a *pullTestApplier) Apply(_ context.Context, key store.TaskKey, fn store.ApplyFunc) (*store.TaskRow, error) {
	key = key.Normalized()
	a.mu.Lock()
	defer a.mu.Unlock()
	cur := a.rows[key].Clone()
	next, err := fn(cur, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if next == nil || (cur != nil && next == cur) {
		return cur.Clone(), nil
	}
	if next == store.DeleteTaskRow {
		delete(a.rows, key)
		return nil, nil
	}
	written := next.Clone()
	if cur == nil {
		written.Generation = 1
	} else {
		written.Generation = cur.Generation + 1
	}
	a.rows[key] = written
	return written.Clone(), nil
}

type pullTestParker struct {
	done chan struct{}
	once sync.Once
}

func (p *pullTestParker) SuspendActor(_ context.Context, _, _ string) error {
	p.once.Do(func() { close(p.done) })
	return nil
}

func TestPullModeTaskRunAndCompletion(t *testing.T) {
	wsDir := t.TempDir()
	origWS := runner.DefaultWorkspacePath
	runner.DefaultWorkspacePath = wsDir
	t.Cleanup(func() { runner.DefaultWorkspacePath = origWS })

	metaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(metaDir, "actor-uid"), []byte("uid-pull-1\n"), 0o600); err != nil {
		t.Fatalf("writing actor-uid: %v", err)
	}
	stateDir := t.TempDir()
	runDir := t.TempDir()

	applier := &pullTestApplier{
		rows: map[store.TaskKey]*store.TaskRow{
			{Atespace: "default", Name: "pull-task"}: {
				Task: &v1alpha1.Task{
					Metadata: &v1alpha1.ObjectMeta{Atespace: "default", Name: "pull-task"},
					Spec: &v1alpha1.TaskSpec{
						Commands: []string{"sh", "-c", "printf 'hello-pull' > \"$AX_OUTPUT_FILE\""},
						Goal:     &v1alpha1.Goal{Text: "write hello-pull"},
					},
					Status: &v1alpha1.TaskStatus{State: v1alpha1.TaskState_TASK_STATE_QUEUED},
				},
				ActorUID: "uid-pull-1",
				Runnable: true,
			},
		},
	}

	parker := &pullTestParker{done: make(chan struct{})}
	srv := runnerhttp.NewServer(runnerhttp.Options{
		TaskApplier: applier,
		ActorParker: parker,
		Verifier: runnerhttp.ActorVerifierFunc(func(_ context.Context, _ *http.Request) (runnerhttp.ActorIdentity, error) {
			return runnerhttp.ActorIdentity{
				Atespace:  "default",
				ActorName: "pull-task",
				ActorUID:  "uid-pull-1",
			}, nil
		}),
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	runCtx, cancel := context.WithCancel(t.Context())
	defer cancel()

	dropPrivs := false
	errCh := make(chan error, 1)
	go func() {
		errCh <- runner.Run(runCtx, runner.Config{
			Port:             freePort(t),
			MetadataPort:     freePort(t),
			PullMode:         true,
			ControlURL:       ts.URL,
			ActorMetadataDir: metaDir,
			StateDir:         stateDir,
			RunDir:           runDir,
			DropPrivileges:   &dropPrivs,
		})
	}()

	select {
	case err := <-errCh:
		t.Fatalf("runner.Run() exited early: %v", err)
	case <-parker.done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for pull-mode task completion and actor park")
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("runner.Run() error = %v", err)
	}
	srv.WaitBackground()

	row, err := applier.GetTaskRow(t.Context(), store.TaskKey{Atespace: "default", Name: "pull-task"})
	if err != nil {
		t.Fatalf("GetTaskRow() error = %v", err)
	}
	if row.Task.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_COMPLETED {
		t.Fatalf("task state = %v, want COMPLETED", row.Task.GetStatus().GetState())
	}
	if got := row.Task.GetStatus().GetOutcome().GetOutput(); got != "hello-pull" {
		t.Fatalf("task output = %q, want %q", got, "hello-pull")
	}
}

func TestPullModeWorkspaceSetup(t *testing.T) {
	wsDir := t.TempDir()
	origWS := runner.DefaultWorkspacePath
	runner.DefaultWorkspacePath = wsDir
	t.Cleanup(func() { runner.DefaultWorkspacePath = origWS })

	metaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(metaDir, "actor-uid"), []byte("uid-setup-1\n"), 0o600); err != nil {
		t.Fatalf("writing actor-uid: %v", err)
	}
	stateDir := t.TempDir()

	srv := runnerhttp.NewServer(runnerhttp.Options{
		Verifier: runnerhttp.ActorVerifierFunc(func(_ context.Context, _ *http.Request) (runnerhttp.ActorIdentity, error) {
			return runnerhttp.ActorIdentity{
				Atespace:  "default",
				ActorName: "ax-ws-setup-test",
				ActorUID:  "uid-setup-1",
			}, nil
		}),
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Atespace: "default", Name: "ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Setup: &v1alpha1.WorkspaceSetup{
				Commands: []string{"printf 'setup-ok' > hello.txt"},
			},
		},
	}
	if err := srv.RegisterSetup(t.Context(), "default", "ax-ws-setup-test", "uid-setup-1", ws); err != nil {
		t.Fatalf("RegisterSetup() error = %v", err)
	}

	runCtx, cancel := context.WithCancel(t.Context())
	defer cancel()

	dropPrivs := false
	errCh := make(chan error, 1)
	go func() {
		errCh <- runner.Run(runCtx, runner.Config{
			Port:             freePort(t),
			MetadataPort:     freePort(t),
			PullMode:         true,
			ControlURL:       ts.URL,
			ActorMetadataDir: metaDir,
			StateDir:         stateDir,
			DropPrivileges:   &dropPrivs,
		})
	}()

	awaitCtx, awaitCancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer awaitCancel()
	if err := srv.AwaitSetup(awaitCtx, "default", "ax-ws-setup-test"); err != nil {
		t.Fatalf("AwaitSetup() error = %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(wsDir, "hello.txt"))
	if err != nil || string(raw) != "setup-ok" {
		t.Fatalf("setup file content = %q (err=%v), want %q", string(raw), err, "setup-ok")
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("runner.Run() error = %v", err)
	}
}
