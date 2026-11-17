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
	"errors"
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
	"github.com/google/ax/internal/tunnel"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestParsePromoteArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    promoteFlags
		wantErr bool
	}{
		{name: "kind and name", args: []string{"task", "demo", "--to", "prod"}, want: promoteFlags{taskName: "demo", to: "prod"}},
		{
			name: "name only, after every other flag",
			args: []string{"--to-server=127.0.0.1:8080", "--to-namespace", "ax", "--to-atespace", "team", "--replace", "--suspend-source", "--no-resume", "--preflight-only", "demo"},
			want: promoteFlags{taskName: "demo", toServer: "127.0.0.1:8080", toNamespace: "ax", toAtespace: "team", replace: true, suspendSource: true, noResume: true, preflightOnly: true},
		},
		{name: "long help", args: []string{"--help"}, want: promoteFlags{help: true}},
		{name: "short help", args: []string{"-h"}, want: promoteFlags{help: true}},
		{name: "no task name", args: []string{"--to", "prod"}, wantErr: true},
		{name: "two task names", args: []string{"demo", "other", "--to", "prod"}, wantErr: true},
		{name: "no target", args: []string{"task", "demo"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePromoteArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parsePromoteArgs(%q) error = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("parsePromoteArgs(%q) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

func TestRunPromote(t *testing.T) {
	// start serves an AX control plane as ax-server does, with its own store
	// and Substrate, and records a tunnel for kubeContext to it in namespace.
	type cluster struct {
		sub   *substratetest.Server
		store store.Store
		srv   *server.Server
		addr  string
	}
	t.Setenv("AX_HOME", t.TempDir())
	start := func(kubeContext, namespace string) cluster {
		c := cluster{sub: substratetest.NewServer(t), store: memory.NewStore()}
		c.srv = server.NewServer(c.store, newTestReconciler(c.sub))
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		c.addr = listener.Addr().String()
		// The tunnel's health check is HTTP/1 and the API is gRPC.
		httpServer := &http.Server{Handler: c.srv.Handler(), Protocols: new(http.Protocols)}
		httpServer.Protocols.SetHTTP1(true)
		httpServer.Protocols.SetUnencryptedHTTP2(true)
		t.Cleanup(func() { _ = httpServer.Close() })
		go func() { _ = httpServer.Serve(listener) }()
		if err := tunnel.SaveTunnel(&tunnel.TunnelInfo{Context: kubeContext, Namespace: namespace, Port: listener.Addr().(*net.TCPAddr).Port}); err != nil {
			t.Fatal(err)
		}
		return c
	}
	source, remote, other := start("local", "ax-system"), start("remote", "ax-system"), start("other", "ax")
	// --to reaches its kube context even when AX_SERVER is set, here to the source.
	t.Setenv("AX_SERVER", source.addr)
	// promote runs `ax -a dev promote task demo` on the source and returns what
	// it prints, to stdout and to stderr.
	promote := func(args ...string) (string, error) {
		output, err := os.CreateTemp(t.TempDir(), "output")
		if err != nil {
			t.Fatal(err)
		}
		defer output.Close()
		stdout, stderr := os.Stdout, os.Stderr
		os.Stdout, os.Stderr = output, output
		defer func() { os.Stdout, os.Stderr = stdout, stderr }()
		runErr := runPromote(source.addr, "dev", append([]string{"task", "demo"}, args...))
		data, err := os.ReadFile(output.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(data), runErr
	}
	taskState := func(c cluster, atespace string) v1alpha1.TaskState {
		task, err := c.srv.GetTask(t.Context(), &v1alpha1.GetTaskRequest{Atespace: atespace, Name: "demo"})
		if err != nil {
			t.Fatal(err)
		}
		return task.GetStatus().GetState()
	}

	// The source runs a task that binds a gateway and a workspace, and whose
	// workspace has finished setting up.
	created := timestamppb.New(time.Unix(1, 0))
	gw := &v1alpha1.Gateway{
		Metadata: &v1alpha1.ObjectMeta{Name: "demo-gw", Atespace: "dev", Uid: "source-uid", CreateTime: created},
		Spec: &v1alpha1.GatewaySpec{Egress: []*ateapipb.EgressRule{{Https: &ateapipb.HttpsRule{
			Hostnames: []string{"api.example.com"},
			Effects: &ateapipb.HttpRuleEffects{ReplaceHeaders: []*ateapipb.CredentialHeader{{
				Header:        "Authorization",
				CredentialUri: "ate-secret://k8s.io/default/ax-system/demo-token/token",
			}}},
		}}}},
	}
	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "demo-ws", Atespace: "dev", Uid: "source-uid", CreateTime: created},
		Spec:     &v1alpha1.WorkspaceSpec{Setup: &v1alpha1.WorkspaceSetup{Commands: []string{"make deps"}}},
	}
	task := &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "demo", Atespace: "dev", Uid: "source-uid", CreateTime: created, Labels: map[string]string{"team": "a"}, Owner: "alice"},
		Spec: &v1alpha1.TaskSpec{
			Profile:   &v1alpha1.Profile{Image: "alpine"},
			Workspace: &v1alpha1.WorkspaceRef{Name: "demo-ws"},
			Gateway:   &v1alpha1.GatewayRef{Name: "demo-gw"},
			SessionId: "source-uid",
		},
		Status: &v1alpha1.TaskStatus{Conditions: []*v1alpha1.Condition{{Type: v1alpha1.ConditionWorkspaceReady, Status: "True"}}},
	}
	if err := source.store.SaveGateway(t.Context(), gw); err != nil {
		t.Fatal(err)
	}
	if err := source.store.SaveWorkspace(t.Context(), ws); err != nil {
		t.Fatal(err)
	}
	if err := source.store.SaveTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if _, err := source.srv.ResumeTask(t.Context(), &v1alpha1.ResumeTaskRequest{Atespace: "dev", Name: "demo"}); err != nil {
		t.Fatal(err)
	}

	// 1. A target that has none of the three gets them, in the task's atespace,
	// and runs the task. The source is only read. A note names the credential
	// the new gateway injects, which is not copied.
	const note = "Note: the target must provide the credentials that gateway demo-gw injects: ate-secret://k8s.io/default/ax-system/demo-token/token\n"
	out, err := promote("--to", "remote")
	if err != nil {
		t.Fatalf("runPromote failed: %v", err)
	}
	wantOutput := "gateway.ax.io/demo-gw created\n" + note + "workspace.ax.io/demo-ws created\ntask.ax.io/demo created\ntask.ax.io/demo resumed\n"
	if out != wantOutput {
		t.Fatalf("expected output %q, got %q", wantOutput, out)
	}
	if got := taskState(remote, "dev"); got != v1alpha1.TaskState_TASK_STATE_WORKING {
		t.Errorf("expected the task working on the target, got %v", got)
	}
	if got := taskState(source, "dev"); got != v1alpha1.TaskState_TASK_STATE_WORKING {
		t.Errorf("expected the task still working on the source, got %v", got)
	}
	// The target has the three specs unchanged.
	gotGW, err := remote.store.GetGateway(t.Context(), "dev", "demo-gw")
	if err != nil || !proto.Equal(gotGW.GetSpec(), gw.GetSpec()) {
		t.Errorf("unexpected gateway on the target: %+v, error %v", gotGW, err)
	}
	gotWS, err := remote.store.GetWorkspace(t.Context(), "dev", "demo-ws")
	if err != nil || !proto.Equal(gotWS.GetSpec(), ws.GetSpec()) {
		t.Errorf("unexpected workspace on the target: %+v, error %v", gotWS, err)
	}
	gotTask, err := remote.store.GetTask(t.Context(), "dev", "demo")
	if err != nil || !proto.Equal(gotTask.GetSpec(), task.GetSpec()) {
		t.Errorf("unexpected task on the target: %+v, error %v", gotTask, err)
	}
	// Labels and owner come along. The target assigns the UIDs and creation
	// times, and works out the task's status itself.
	if meta := gotTask.GetMetadata(); !reflect.DeepEqual(meta.GetLabels(), task.GetMetadata().GetLabels()) || meta.GetOwner() != "alice" {
		t.Errorf("unexpected task metadata on the target: %+v", meta)
	}
	for kind, meta := range map[string]*v1alpha1.ObjectMeta{"gateway": gotGW.GetMetadata(), "workspace": gotWS.GetMetadata(), "task": gotTask.GetMetadata()} {
		if meta.GetUid() == "" || meta.GetUid() == "source-uid" || proto.Equal(meta.GetCreateTime(), created) {
			t.Errorf("expected the %s's UID and creation time to come from the target, got %+v", kind, meta)
		}
	}
	for _, c := range gotTask.GetStatus().GetConditions() {
		if c.GetType() == v1alpha1.ConditionWorkspaceReady && c.GetStatus() == "True" {
			t.Errorf("expected the target to check the workspace itself, got the source's condition %+v", c)
		}
	}

	// 2. A target that already has the three keeps its own gateway, accepts the
	// same workspace, and replaces the task only when told to.
	gotGW.Spec = &v1alpha1.GatewaySpec{}
	if err := remote.store.SaveGateway(t.Context(), gotGW); err != nil {
		t.Fatal(err)
	}
	if _, err := promote("--to", "remote"); err == nil || !strings.Contains(err.Error(), "pass --replace") {
		t.Fatalf("expected an error that points to --replace, got %v", err)
	}
	out, err = promote("--to", "remote", "--replace", "--no-resume")
	if err != nil {
		t.Fatalf("runPromote failed: %v", err)
	}
	wantOutput = "gateway.ax.io/demo-gw reused\nworkspace.ax.io/demo-ws unchanged\ntask.ax.io/demo replaced\n"
	if out != wantOutput {
		t.Fatalf("expected output %q, got %q", wantOutput, out)
	}
	if got := taskState(remote, "dev"); got != v1alpha1.TaskState_TASK_STATE_SUSPENDED {
		t.Errorf("expected the new task suspended on the target, got %v", got)
	}
	if reused, err := remote.store.GetGateway(t.Context(), "dev", "demo-gw"); err != nil || !proto.Equal(reused.GetSpec(), gotGW.GetSpec()) {
		t.Errorf("expected the target's gateway unchanged, got %+v, error %v", reused, err)
	}

	// 3. A workspace that differs on the target stops the promotion before the
	// target is changed. AX runs in another namespace on this target.
	if err := other.store.SaveWorkspace(t.Context(), &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "demo-ws", Atespace: "dev"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := promote("--to", "other", "--to-namespace", "ax"); err == nil || !strings.Contains(err.Error(), "different spec") {
		t.Fatalf("expected an error about the workspace's spec, got %v", err)
	}
	if _, err := other.store.GetGateway(t.Context(), "dev", "demo-gw"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected no gateway on the target, got error %v", err)
	}

	// 4. --preflight-only reports what a promotion into --to-atespace would do
	// and leaves the target alone; without it the three land in that atespace.
	out, err = promote("--to", "other", "--to-namespace", "ax", "--to-atespace", "team", "--preflight-only")
	if err != nil {
		t.Fatalf("runPromote failed: %v", err)
	}
	wantOutput = "gateway.ax.io/demo-gw created (preflight only)\n" + note + "workspace.ax.io/demo-ws created (preflight only)\ntask.ax.io/demo created (preflight only)\ntask.ax.io/demo resumed (preflight only)\n"
	if out != wantOutput {
		t.Fatalf("expected output %q, got %q", wantOutput, out)
	}
	if _, err := other.store.GetGateway(t.Context(), "team", "demo-gw"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected no gateway on the target, got error %v", err)
	}
	if _, err := promote("--to", "other", "--to-namespace", "ax", "--to-atespace", "team"); err != nil {
		t.Fatalf("runPromote failed: %v", err)
	}
	if got := taskState(other, "team"); got != v1alpha1.TaskState_TASK_STATE_WORKING {
		t.Errorf("expected the task working in the target atespace, got %v", got)
	}

	// 5. The source is not a target, even with --replace, which would delete
	// the task that is being promoted.
	if _, err := promote("--to-server", source.addr, "--replace"); err == nil || !strings.Contains(err.Error(), "source and target must differ") {
		t.Fatalf("expected an error that the source and target must differ, got %v", err)
	}

	// 6. --suspend-source suspends the task on the source once the target has
	// created it, and before the target resumes it.
	full := start("full", "ax-system")
	full.sub.SetErrors(substratetest.Errors{CreateActor: status.Error(codes.InvalidArgument, "refused")})
	if _, err := promote("--to", "full", "--suspend-source"); err == nil {
		t.Fatal("expected runPromote to fail when the target cannot create the task")
	}
	if got := taskState(source, "dev"); got != v1alpha1.TaskState_TASK_STATE_WORKING {
		t.Errorf("expected the task still working on the source, got %v", got)
	}
	full.sub.SetErrors(substratetest.Errors{ResumeActor: status.Error(codes.ResourceExhausted, "no worker has room for the actor")})
	out, err = promote("--to", "full", "--suspend-source", "--replace")
	if err == nil {
		t.Fatal("expected runPromote to fail when the target cannot resume the task")
	}
	wantOutput = "gateway.ax.io/demo-gw reused\nworkspace.ax.io/demo-ws unchanged\ntask.ax.io/demo replaced\ntask.ax.io/demo suspended on the source\n"
	if out != wantOutput {
		t.Fatalf("expected output %q, got %q", wantOutput, out)
	}
	if got := taskState(source, "dev"); got != v1alpha1.TaskState_TASK_STATE_SUSPENDED {
		t.Errorf("expected the task suspended on the source, got %v", got)
	}
}
