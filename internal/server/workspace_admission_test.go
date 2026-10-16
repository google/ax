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

package server_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/ax/internal/server"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type workspaceAdmissionStore struct {
	store.Store
	workspaceErr error
	failName     string
	saves        int
	reads        []string
}

func (s *workspaceAdmissionStore) GetWorkspace(ctx context.Context, atespace, name string) (*v1alpha1.Workspace, error) {
	s.reads = append(s.reads, atespace+"/"+name)
	if name == s.failName {
		return nil, s.workspaceErr
	}
	return s.Store.GetWorkspace(ctx, atespace, name)
}

func (s *workspaceAdmissionStore) SaveTask(ctx context.Context, task *v1alpha1.Task) error {
	s.saves++
	return s.Store.SaveTask(ctx, task)
}

type workspaceAdmissionReconciler struct {
	calls      int
	workspaces []*v1alpha1.Workspace
}

func (r *workspaceAdmissionReconciler) Reconcile(_ context.Context, task *v1alpha1.Task, workspaces ...*v1alpha1.Workspace) (*v1alpha1.Task, error) {
	r.calls++
	r.workspaces = workspaces
	return task, nil
}

func (*workspaceAdmissionReconciler) ReconcileDelete(context.Context, string, string) error {
	return nil
}

func TestCreateTask_WorkspaceAdmission(t *testing.T) {
	cases := []struct {
		name              string
		scope             string
		refs              []string
		present           []string
		failName          string
		storeErr          error
		wantCode          codes.Code
		wantMessage       string
		withoutReconciler bool
		withoutSpec       bool
	}{
		{name: "missing first", scope: "team-a", refs: []string{"missing", "present"}, present: []string{"team-a/present"}, wantCode: codes.FailedPrecondition, wantMessage: `workspace "missing" not found in atespace "team-a"`},
		{name: "missing later", scope: "team-a", refs: []string{"present", "missing"}, present: []string{"team-a/present"}, wantCode: codes.FailedPrecondition, wantMessage: `workspace "missing" not found in atespace "team-a"`},
		{name: "other atespace", scope: "team-a", refs: []string{"present"}, present: []string{"team-b/present"}, wantCode: codes.FailedPrecondition, wantMessage: `workspace "present" not found in atespace "team-a"`},
		{name: "default missing", refs: []string{"missing"}, wantCode: codes.FailedPrecondition, wantMessage: `workspace "missing" not found in atespace "default"`},
		{name: "wrapped not found", refs: []string{"missing"}, failName: "missing", storeErr: fmt.Errorf("lookup: %w", store.ErrNotFound), wantCode: codes.FailedPrecondition, wantMessage: `workspace "missing" not found in atespace "default"`},
		{name: "backend error after valid", scope: "team-a", refs: []string{"present", "broken"}, present: []string{"team-a/present"}, failName: "broken", storeErr: errors.New("backend unavailable"), wantCode: codes.Internal, wantMessage: `getting workspace "broken" in atespace "team-a": backend unavailable`},
		{name: "without reconciler missing", refs: []string{"missing"}, wantCode: codes.FailedPrecondition, withoutReconciler: true},
		{name: "all present in order", scope: "team-a", refs: []string{"second", "first"}, present: []string{"team-a/first", "team-a/second"}},
		{name: "default present", refs: []string{"present"}, present: []string{"default/present"}},
		{name: "without reconciler present", refs: []string{"present"}, present: []string{"default/present"}, withoutReconciler: true},
		{name: "no references"},
		{name: "no spec", withoutSpec: true},
		{name: "backend error without reconciler", refs: []string{"broken"}, failName: "broken", storeErr: errors.New("backend unavailable"), wantCode: codes.Internal, wantMessage: `getting workspace "broken" in atespace "default": backend unavailable`, withoutReconciler: true},
		{name: "duplicate binding", refs: []string{"present", "present"}, present: []string{"default/present"}, wantCode: codes.InvalidArgument, wantMessage: `workspace "present" is bound more than once`},
	}
	for _, transport := range []string{"direct", "grpc"} {
		t.Run(transport, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					t.Cleanup(cancel)
					base := memory.NewStore()
					for _, key := range tc.present {
						parts := strings.Split(key, "/")
						if err := base.SaveWorkspace(ctx, &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Atespace: parts[0], Name: parts[1]}, Spec: &v1alpha1.WorkspaceSpec{}}); err != nil {
							t.Fatal(err)
						}
					}
					st := &workspaceAdmissionStore{Store: base, failName: tc.failName, workspaceErr: tc.storeErr}
					rec := &workspaceAdmissionReconciler{}
					opts := server.Options{}
					if !tc.withoutReconciler {
						opts.Reconciler = rec
					}
					srv := server.NewServer(st, opts)
					create := srv.CreateTask
					if transport == "grpc" {
						ln, err := net.Listen("tcp", "127.0.0.1:0")
						if err != nil {
							t.Fatal(err)
						}
						done := make(chan error, 1)
						go func() { done <- srv.GRPCServer().Serve(ln) }()
						t.Cleanup(func() {
							srv.GRPCServer().Stop()
							if err := <-done; err != nil {
								t.Errorf("Serve: %v", err)
							}
						})
						conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() {
							if err := conn.Close(); err != nil {
								t.Errorf("Close: %v", err)
							}
						})
						client := v1alpha1.NewAXClient(conn)
						create = func(ctx context.Context, req *v1alpha1.CreateTaskRequest) (*v1alpha1.Task, error) {
							return client.CreateTask(ctx, req)
						}
					}
					task := &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: "admission-task", Atespace: tc.scope}, Spec: &v1alpha1.TaskSpec{Image: "alpine"}}
					for i, name := range tc.refs {
						task.Spec.Workspaces = append(task.Spec.Workspaces, &v1alpha1.WorkspaceRef{Name: name, Path: fmt.Sprintf("/workspace/%d", i)})
					}
					if tc.withoutSpec {
						task.Spec = nil
					}
					originalSpec := proto.Clone(task.Spec)
					got, err := create(ctx, &v1alpha1.CreateTaskRequest{Task: task})
					if status.Code(err) != tc.wantCode {
						t.Errorf("CreateTask = %v, want %v", err, tc.wantCode)
					}
					if tc.wantMessage != "" && !strings.Contains(status.Convert(err).Message(), tc.wantMessage) {
						t.Errorf("error = %v, want %q", err, tc.wantMessage)
					}
					scope := tc.scope
					if scope == "" {
						scope = "default"
					}
					saved, savedErr := base.GetTask(ctx, scope, "admission-task")
					if tc.wantCode != codes.OK {
						if got != nil {
							t.Errorf("rejected task = %v", got)
						}
						if st.saves != 0 || rec.calls != 0 {
							t.Errorf("rejected task: saves=%d reconciles=%d", st.saves, rec.calls)
						}
						if !errors.Is(savedErr, store.ErrNotFound) {
							t.Errorf("GetTask after rejection = %v, want ErrNotFound", savedErr)
						}
						return
					}
					if err != nil || got == nil || savedErr != nil {
						t.Fatalf("valid task: response=%v err=%v storage=%v", got, err, savedErr)
					}
					if st.saves != 1 {
						t.Errorf("SaveTask calls = %d, want 1", st.saves)
					}
					if !proto.Equal(saved.Spec, originalSpec) || !proto.Equal(got.Spec, originalSpec) {
						t.Errorf("workspace bindings changed")
					}
					if got.Metadata.Atespace != scope || got.Status.Phase != "Suspended" {
						t.Errorf("task defaults = %v", got)
					}
					var wantReads []string
					for _, name := range tc.refs {
						wantReads = append(wantReads, scope+"/"+name)
					}
					if !reflect.DeepEqual(st.reads, wantReads) {
						t.Errorf("workspace reads = %v, want %v", st.reads, wantReads)
					}
					if !tc.withoutReconciler {
						if rec.calls != 1 || len(rec.workspaces) != len(tc.refs) {
							t.Fatalf("reconciler calls=%d workspaces=%v", rec.calls, rec.workspaces)
						}
						for i, ws := range rec.workspaces {
							if ws.Metadata.Name != tc.refs[i] || ws.Metadata.Atespace != scope {
								t.Errorf("reconciler workspace %d = %v", i, ws)
							}
						}
					}
				})
			}
		})
	}
}
