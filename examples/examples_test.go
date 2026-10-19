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

package examples

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"gopkg.in/yaml.v3"
)

func TestExampleWorkspaceOrder(t *testing.T) {
	paths, err := filepath.Glob("*.yaml")
	if err != nil || len(paths) == 0 {
		t.Fatalf("finding examples: paths=%v, error=%v", paths, err)
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { file.Close() })
			if err := checkWorkspaceOrder(file); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// checkWorkspaceOrder checks dependencies within each manifest's atespace.
func checkWorkspaceOrder(input io.Reader) error {
	decoder := yaml.NewDecoder(input)
	type key struct{ atespace, name string }
	scopedKey := func(atespace, name string) key {
		if atespace == "" {
			atespace = "default"
		}
		return key{atespace, name}
	}
	defined := make(map[key]bool)
	for index := 1; ; index++ {
		var document yaml.Node
		if err := decoder.Decode(&document); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("document %d: %w", index, err)
		}
		var head struct {
			Kind string `yaml:"kind"`
		}
		if err := document.Decode(&head); err != nil {
			return err
		}
		switch head.Kind {
		case v1alpha1.KindWorkspace:
			var workspace v1alpha1.Workspace
			if err := document.Decode(&workspace); err != nil {
				return err
			}
			if workspace.Metadata == nil {
				return fmt.Errorf("document %d: missing Workspace metadata", index)
			}
			defined[scopedKey(workspace.Metadata.Atespace, workspace.Metadata.Name)] = true
		case v1alpha1.KindTask:
			var task v1alpha1.Task
			if err := document.Decode(&task); err != nil {
				return fmt.Errorf("document %d: decoding Task: %w", index, err)
			}
			if task.Metadata == nil || task.Spec == nil {
				return fmt.Errorf("document %d: missing Task metadata or spec", index)
			}
			for _, reference := range task.Spec.Workspaces {
				if reference == nil {
					return fmt.Errorf("document %d: Task %s/%s has a nil Workspace reference", index, task.Metadata.Atespace, task.Metadata.Name)
				}
				if !defined[scopedKey(task.Metadata.Atespace, reference.Name)] {
					return fmt.Errorf("document %d: Task %s/%s references Workspace %s before it is defined in that atespace", index, task.Metadata.Atespace, task.Metadata.Name, reference.Name)
				}
			}
		}
	}
}

func TestWorkspaceOrderScope(t *testing.T) {
	workspace := func(scope, name string) string {
		return fmt.Sprintf("kind: Workspace\nmetadata: {atespace: %s, name: %s}\nspec: {}\n---\n", scope, name)
	}
	task := "kind: Task\nmetadata: {atespace: one, name: task}\nspec:\n  workspaces:\n    - {name: data, path: /workspace}\n"
	implicitWorkspace := strings.Replace(workspace("default", "data"), "atespace: default, ", "", 1)
	implicitTask := strings.Replace(task, "atespace: one, ", "", 1)
	defaultTask := strings.Replace(task, "atespace: one", "atespace: default", 1)
	for _, tc := range []struct {
		name, manifest string
		wantError      bool
	}{
		{"implicit Workspace", implicitWorkspace + defaultTask, false},
		{"implicit Task", workspace("default", "data") + implicitTask, false},
		{"both implicit", implicitWorkspace + implicitTask, false},
		{"nil reference", workspace("one", "data") + task + "    - null\n", true},
		{"same scope", workspace("one", "data") + task, false},
		{"other scope", workspace("two", "data") + task, true},
		{"wrong name", workspace("one", "other") + task, true},
		{"task first", task + "---\n" + workspace("one", "data"), true},
		{"both scopes", workspace("two", "data") + workspace("one", "data") + task, false},
		{"no references", "kind: Task\nmetadata: {atespace: one, name: task}\nspec: {}\n", false},
		{"multiple references", workspace("one", "data") + task + "    - {name: other, path: /other}\n", true},
		{"all references", workspace("one", "data") + workspace("one", "other") + task + "    - {name: other, path: /other}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkWorkspaceOrder(strings.NewReader(tc.manifest))
			if tc.name == "nil reference" && (err == nil || !strings.Contains(err.Error(), "decoding Task:") || !strings.Contains(err.Error(), "unexpected token null")) {
				t.Fatalf("expected a descriptive nil-reference error, got %v", err)
			}
			if (err != nil) != tc.wantError {
				t.Fatalf("checkWorkspaceOrder() = %v, want error %v", err, tc.wantError)
			}
		})
	}
}
