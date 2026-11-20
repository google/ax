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

package main

import (
	"bytes"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/ax/internal/server"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/internal/substrate/substratetest"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

func TestParseForkArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    forkFlags
		wantErr bool
	}{
		{
			name: "kind, names, goal, and repeatable flags",
			args: []string{"task", "turn1", "turn2", "--goal", "add multiply", "--env", "A=1", "--env", "B=2=3", "--label", "step=2", "--session-id", "sess-1", "--no-resume"},
			want: forkFlags{
				sourceName: "turn1",
				targetName: "turn2",
				goal:       "add multiply",
				goalSet:    true,
				sessionID:  "sess-1",
				env:        []*v1alpha1.EnvVar{{Name: "A", Value: "1"}, {Name: "B", Value: "2=3"}},
				labels:     map[string]string{"step": "2"},
				noResume:   true,
			},
		},
		{
			name: "names without kind and trailing command",
			args: []string{"--goal=next", "turn1", "turn2", "--", "sh", "-c", "echo ok"},
			want: forkFlags{
				sourceName: "turn1",
				targetName: "turn2",
				goal:       "next",
				goalSet:    true,
				commands:   []string{"sh", "-c", "echo ok"},
			},
		},
		{name: "help", args: []string{"--help"}, want: forkFlags{help: true}},
		{name: "missing target", args: []string{"task", "turn1"}, wantErr: true},
		{name: "same name", args: []string{"task", "turn1", "turn1"}, wantErr: true},
		{name: "bad env", args: []string{"turn1", "turn2", "--env", "NOEQUALS"}, wantErr: true},
		{name: "bad label", args: []string{"turn1", "turn2", "--label", "=val"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseForkArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseForkArgs(%q) err = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseForkArgs(%q) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

func TestRunFork(t *testing.T) {
	ctx := t.Context()
	sub := substratetest.NewServer(t)
	memStore := memory.NewStore()
	applier := server.TaskApplierForStore(memStore)
	srv := server.NewServer(memStore, newTestReconciler(sub), server.Options{TaskApplier: applier})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := &http.Server{Handler: srv.Handler(), Protocols: new(http.Protocols)}
	httpSrv.Protocols.SetHTTP1(true)
	httpSrv.Protocols.SetUnencryptedHTTP2(true)
	t.Cleanup(func() { _ = httpSrv.Close() })
	go func() { _ = httpSrv.Serve(ln) }()

	if _, err := srv.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "turn-1", Atespace: "default"},
		Spec: &v1alpha1.TaskSpec{
			Profile:   &v1alpha1.Profile{Image: "ghcr.io/example/agent@sha256:aaaa"},
			SessionId: "sess-demo",
			Goal:      &v1alpha1.Goal{Text: "turn 1 goal"},
		},
	}}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if _, err := applier.Apply(ctx, store.TaskKey{Atespace: "default", Name: "turn-1"}, func(current *store.TaskRow, _ time.Time) (*store.TaskRow, error) {
		next := current.Clone()
		next.Task.Status.State = v1alpha1.TaskState_TASK_STATE_COMPLETED
		next.Task.Status.Outcome = &v1alpha1.TaskOutcome{Result: v1alpha1.TaskResult_TASK_RESULT_SUCCEEDED}
		return next, nil
	}); err != nil {
		t.Fatalf("mark COMPLETED: %v", err)
	}

	run := func(args ...string) (string, error) {
		outFile, err := os.CreateTemp(t.TempDir(), "fork-out")
		if err != nil {
			t.Fatal(err)
		}
		defer outFile.Close()
		prev := os.Stdout
		os.Stdout = outFile
		defer func() { os.Stdout = prev }()
		runErr := runFork(ln.Addr().String(), "default", args)
		data, err := os.ReadFile(outFile.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(data), runErr
	}

	out, err := run("task", "turn-1", "turn-2", "--goal", "turn 2 goal")
	if err != nil {
		t.Fatalf("runFork: %v\n%s", err, out)
	}
	if !strings.Contains(out, "task.ax.io/turn-2 forked from turn-1") || !strings.Contains(out, "task.ax.io/turn-2 resumed") {
		t.Errorf("unexpected output:\n%s", out)
	}

	turn2, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: "default", Name: "turn-2"})
	if err != nil {
		t.Fatalf("GetTask(turn-2): %v", err)
	}
	if turn2.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_WORKING {
		t.Errorf("turn-2 state = %v, want WORKING", turn2.GetStatus().GetState())
	}
	if turn2.GetSpec().GetSessionId() != "sess-demo" || turn2.GetSpec().GetGoal().GetText() != "turn 2 goal" {
		t.Errorf("turn-2 spec = %+v", turn2.GetSpec())
	}

	var desc bytes.Buffer
	describeTask(&desc, turn2)
	if !strings.Contains(desc.String(), "Parent Task:  turn-1") {
		t.Errorf("describeTask missing Parent Task:\n%s", desc.String())
	}

	// --no-resume leaves the forked task suspended.
	out, err = run("task", "turn-1", "turn-3", "--no-resume")
	if err != nil {
		t.Fatalf("runFork --no-resume: %v\n%s", err, out)
	}
	if strings.Contains(out, "resumed") {
		t.Errorf("expected no resume message with --no-resume, got:\n%s", out)
	}
	turn3, err := srv.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: "default", Name: "turn-3"})
	if err != nil {
		t.Fatalf("GetTask(turn-3): %v", err)
	}
	if turn3.GetStatus().GetState() != v1alpha1.TaskState_TASK_STATE_SUSPENDED {
		t.Errorf("turn-3 state = %v, want SUSPENDED", turn3.GetStatus().GetState())
	}
}
