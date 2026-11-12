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

package memory_test

import (
	"math"
	"slices"
	"testing"

	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

func TestListPagination(t *testing.T) {
	ctx := t.Context()
	s := memory.NewStore()
	for _, name := range []string{"c", "a", "b"} {
		if err := s.SaveWorkspace(ctx, &v1alpha1.Workspace{
			Metadata: &v1alpha1.ObjectMeta{Name: "ws-" + name, Atespace: "default"},
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveTask(ctx, &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: "task-" + name, Atespace: "default"},
		}); err != nil {
			t.Fatal(err)
		}
	}

	wsNames := func(items []*v1alpha1.Workspace) []string {
		var out []string
		for _, ws := range items {
			out = append(out, ws.GetMetadata().GetName())
		}
		return out
	}
	taskNames := func(items []*v1alpha1.Task) []string {
		var out []string
		for _, task := range items {
			out = append(out, task.GetMetadata().GetName())
		}
		return out
	}

	page, err := s.ListWorkspaces(ctx, "default", 1, 1)
	if err != nil || !slices.Equal(wsNames(page), []string{"ws-b"}) {
		t.Fatalf("ListWorkspaces(limit=1, offset=1) = %v, %v; want [ws-b]", wsNames(page), err)
	}

	// Negative offset clamps to 0 and limit <= 0 applies default page limit.
	allWS, err := s.ListWorkspaces(ctx, "default", 0, -5)
	if err != nil || !slices.Equal(wsNames(allWS), []string{"ws-a", "ws-b", "ws-c"}) {
		t.Fatalf("ListWorkspaces(limit=0, offset=-5) = %v, %v; want [ws-a ws-b ws-c]", wsNames(allWS), err)
	}
	allTasks, err := s.ListTasks(ctx, "default", 0, -1)
	if err != nil || !slices.Equal(taskNames(allTasks), []string{"task-a", "task-b", "task-c"}) {
		t.Fatalf("ListTasks(limit=0, offset=-1) = %v, %v; want [task-a task-b task-c]", taskNames(allTasks), err)
	}

	empty, err := s.ListWorkspaces(ctx, "default", 10, 99)
	if err != nil || len(empty) != 0 {
		t.Fatalf("ListWorkspaces(offset=99) = %v, %v; want empty", wsNames(empty), err)
	}

	// Large limit + positive offset must not overflow int64 and panic.
	maxWS, err := s.ListWorkspaces(ctx, "default", math.MaxInt64, 1)
	if err != nil || !slices.Equal(wsNames(maxWS), []string{"ws-b", "ws-c"}) {
		t.Fatalf("ListWorkspaces(limit=MaxInt64, offset=1) = %v, %v; want [ws-b ws-c]", wsNames(maxWS), err)
	}
	maxTasks, err := s.ListTasks(ctx, "default", math.MaxInt64, 1)
	if err != nil || !slices.Equal(taskNames(maxTasks), []string{"task-b", "task-c"}) {
		t.Fatalf("ListTasks(limit=MaxInt64, offset=1) = %v, %v; want [task-b task-c]", taskNames(maxTasks), err)
	}
}
