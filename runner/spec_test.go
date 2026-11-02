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

package runner

import (
	"testing"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

// A newer ax-server may send Task and Workspace fields this runner doesn't
// know; they must not stop the task from starting.
func TestLoadSpec_IgnoresUnknownFields(t *testing.T) {
	t.Setenv("AX_TASK_YAML", "metadata:\n  name: t\nspec:\n  image: img\n  egress:\n  - host: example.com\n")
	var task v1alpha1.Task
	if err := LoadSpec("", "AX_TASK_YAML", &task); err != nil {
		t.Fatalf("loadSpec: %v", err)
	}
	if task.GetMetadata().GetName() != "t" || task.GetSpec().GetImage() != "img" {
		t.Errorf("unexpected task %v", &task)
	}
}

func TestLoadSpec_Empty(t *testing.T) {
	t.Setenv("AX_TASK_YAML", "\n")
	var task v1alpha1.Task
	if err := LoadSpec("", "AX_TASK_YAML", &task); err != nil {
		t.Fatalf("loadSpec: %v", err)
	}
	if task.GetMetadata() != nil {
		t.Errorf("expected no task, got %v", &task)
	}
}

func TestLoadSpec_WorkspaceIgnoresUnknownFields(t *testing.T) {
	t.Setenv("AX_WORKSPACE_YAML", "metadata:\n  name: ws\nspec:\n  fieldFromTheFuture: 1\n")
	var ws v1alpha1.Workspace
	if err := LoadSpec("", "AX_WORKSPACE_YAML", &ws); err != nil {
		t.Fatalf("loadSpec: %v", err)
	}
	if ws.GetMetadata().GetName() != "ws" {
		t.Errorf("unexpected workspace %v", &ws)
	}
}
