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

package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

func TestStoredStatus(t *testing.T) {
	in := &v1alpha1.TaskStatus{
		State:            v1alpha1.TaskState_TASK_STATE_WORKING,
		Actor:            "actor",
		ActorUid:         "uid-1",
		AteActorTemplate: "task-tmpl-01234567",
		Attempt:          1,
		Generation:       2,
		Conditions:       []*v1alpha1.Condition{{Type: "Ready"}, {Type: "WorkspaceReady"}},
	}
	got := storedStatus(in)
	if got.State != v1alpha1.TaskState_TASK_STATE_UNSPECIFIED || got.Actor != "" || got.ActorUid != "" || got.AteActorTemplate != "task-tmpl-01234567" {
		t.Errorf("storedStatus = %v, want actor/actorUid/state stripped and AteActorTemplate kept", got)
	}
	if len(got.Conditions) != 1 || got.Conditions[0].Type != "WorkspaceReady" {
		t.Errorf("conditions = %v, want only WorkspaceReady", got.Conditions)
	}
	if in.Actor != "actor" || in.ActorUid != "uid-1" || len(in.Conditions) != 2 {
		t.Errorf("storedStatus modified its input: %v", in)
	}

	in = &v1alpha1.TaskStatus{State: v1alpha1.TaskState_TASK_STATE_FAILED, Conditions: []*v1alpha1.Condition{{Type: "Ready"}}}
	if got := storedStatus(in); got.State != v1alpha1.TaskState_TASK_STATE_FAILED || len(got.Conditions) != 1 {
		t.Errorf("storedStatus(%v) = %v, want AX state and its conditions kept", in, got)
	}
	if got := storedStatus(nil); got == nil {
		t.Error("storedStatus(nil) = nil, want empty status")
	}
	for _, state := range []v1alpha1.TaskState{
		v1alpha1.TaskState_TASK_STATE_QUEUED,
		v1alpha1.TaskState_TASK_STATE_FAILED,
		v1alpha1.TaskState_TASK_STATE_COMPLETED,
	} {
		t.Run(state.String(), func(t *testing.T) {
			if got := storedStatus(&v1alpha1.TaskStatus{State: state, ActorUid: "uid-1"}); got.ActorUid != "" {
				t.Errorf("stored actor UID = %q, want empty", got.ActorUid)
			}
		})
	}
}

// TestTasksGoThroughHelpers fails if any code in the package other than the
// task helpers calls a task method on s.store, which would skip the split
// between stored and Substrate-owned status.
func TestTasksGoThroughHelpers(t *testing.T) {
	helpers := map[string]bool{
		"getStoredTask": true, "getTask": true, "listTasks": true,
		"saveTask": true, "updateTask": true, "setTaskDeleteTime": true, "deleteTask": true,
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || (fn.Recv != nil && helpers[fn.Name.Name]) {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || !strings.Contains(sel.Sel.Name, "Task") {
					return true
				}
				if recv, ok := sel.X.(*ast.SelectorExpr); ok && recv.Sel.Name == "store" {
					t.Errorf("%s: %s calls store.%s directly; use a task helper", fset.Position(sel.Pos()), fn.Name.Name, sel.Sel.Name)
				}
				return true
			})
		}
	}
}
