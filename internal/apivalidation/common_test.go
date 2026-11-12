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

package apivalidation_test

import (
	"strings"
	"testing"

	"github.com/google/ax/internal/apivalidation"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

func TestValidateName(t *testing.T) {
	valid := []string{
		"a",
		"0",
		"task123",
		"my-task",
		"a-b-c",
		"123-abc",
		strings.Repeat("a", 63),
	}
	for _, name := range valid {
		if err := apivalidation.ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{
		"",
		"Task-With-Caps",
		"my_task",
		"my.task",
		"-leading-dash",
		"trailing-dash-",
		"with space",
		"tab\tchar",
		"ünïcödé",
		"slash/name",
		strings.Repeat("a", 64),
	}
	for _, name := range invalid {
		if err := apivalidation.ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", name)
		}
	}
}

func TestValidateObjectMeta(t *testing.T) {
	tests := []struct {
		name    string
		meta    *v1alpha1.ObjectMeta
		wantErr string
	}{
		{name: "nil metadata", wantErr: "metadata.name"},
		{name: "missing name", meta: &v1alpha1.ObjectMeta{Atespace: "default"}, wantErr: `metadata.name: invalid value ""`},
		{name: "name only", meta: &v1alpha1.ObjectMeta{Name: "task123"}},
		{name: "name and atespace", meta: &v1alpha1.ObjectMeta{Name: "task123", Atespace: "team-a"}},
		{name: "uppercase name", meta: &v1alpha1.ObjectMeta{Name: "Task-With-Caps"}, wantErr: `metadata.name: invalid value "Task-With-Caps"`},
		{name: "underscore in name", meta: &v1alpha1.ObjectMeta{Name: "my_task"}, wantErr: "metadata.name"},
		{name: "name too long", meta: &v1alpha1.ObjectMeta{Name: strings.Repeat("x", 64)}, wantErr: "metadata.name"},
		{name: "uppercase atespace", meta: &v1alpha1.ObjectMeta{Name: "task123", Atespace: "Default"}, wantErr: `metadata.atespace: invalid value "Default"`},
		{name: "dotted atespace", meta: &v1alpha1.ObjectMeta{Name: "task123", Atespace: "team.a"}, wantErr: "metadata.atespace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := apivalidation.ValidateObjectMeta(tt.meta)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}

	// Every kind is validated the same way.
	if err := apivalidation.ValidateTask(&v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: "Bad"}}); err == nil {
		t.Error("ValidateTask accepted an invalid name")
	}
	if err := apivalidation.ValidateWorkspace(&v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "Bad"}}); err == nil {
		t.Error("ValidateWorkspace accepted an invalid name")
	}
	if err := apivalidation.ValidateModel(&v1alpha1.Model{Metadata: &v1alpha1.ObjectMeta{Name: "Bad"}}); err == nil {
		t.Error("ValidateModel accepted an invalid name")
	}
	if err := apivalidation.ValidateGateway(&v1alpha1.Gateway{Metadata: &v1alpha1.ObjectMeta{Name: "Bad"}}); err == nil {
		t.Error("ValidateGateway accepted an invalid name")
	}
}

func TestValidateResources(t *testing.T) {
	limits := func(cpu, memory string) *v1alpha1.ResourcesSpec {
		return &v1alpha1.ResourcesSpec{Limits: &v1alpha1.ResourceList{Cpu: cpu, Memory: memory}}
	}
	tests := []struct {
		name    string
		reqs    *v1alpha1.ResourcesSpec
		wantErr string
	}{
		{name: "nil"},
		{name: "empty", reqs: &v1alpha1.ResourcesSpec{}},
		{name: "empty limits", reqs: limits("", "")},
		{name: "docs example", reqs: limits("2", "4Gi")},
		{name: "millicores", reqs: limits("500m", "")},
		{name: "fractional cores", reqs: limits("1.5", "")},
		{name: "leading dot", reqs: limits(".5", "")},
		{name: "exponent", reqs: limits("2e0", "1e9")},
		{name: "largest cpu", reqs: limits("999999m", "")},
		{name: "decimal SI memory", reqs: limits("", "512M")},
		{name: "binary SI memory", reqs: limits("", "1Ti")},
		{name: "small units", reqs: limits("1n", "1u")},
		{name: "cpu zero", reqs: limits("0", ""), wantErr: `spec.resources.limits.cpu: "0" must be greater than zero`},
		{name: "cpu zero millicores", reqs: limits("0m", ""), wantErr: "must be greater than zero"},
		{name: "cpu negative", reqs: limits("-1", ""), wantErr: "must be greater than zero"},
		{name: "cpu at bound", reqs: limits("1000", ""), wantErr: `spec.resources.limits.cpu: "1000" must be less than 1000 cores`},
		{name: "cpu over bound via suffix", reqs: limits("1k", ""), wantErr: "must be less than 1000 cores"},
		{name: "cpu over bound via exponent", reqs: limits("1e3", ""), wantErr: "must be less than 1000 cores"},
		{name: "cpu over bound via binary suffix", reqs: limits("1Ki", ""), wantErr: "must be less than 1000 cores"},
		{name: "cpu not a quantity", reqs: limits("two", ""), wantErr: `spec.resources.limits.cpu: invalid quantity "two"`},
		{name: "cpu bad suffix", reqs: limits("2cores", ""), wantErr: "invalid quantity"},
		{name: "cpu double dot", reqs: limits("1.2.3", ""), wantErr: "invalid quantity"},
		{name: "cpu whitespace", reqs: limits(" 2", ""), wantErr: "invalid quantity"},
		{name: "memory zero", reqs: limits("", "0Gi"), wantErr: `spec.resources.limits.memory: "0Gi" must be greater than zero`},
		{name: "memory negative", reqs: limits("", "-4Gi"), wantErr: "must be greater than zero"},
		{name: "memory lowercase suffix", reqs: limits("", "4gi"), wantErr: `spec.resources.limits.memory: invalid quantity "4gi"`},
		{name: "memory bytes suffix", reqs: limits("", "4GiB"), wantErr: "invalid quantity"},
		{name: "memory exponent out of range", reqs: limits("", "1e100"), wantErr: "exponent out of range"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := apivalidation.ValidateResources(tt.reqs)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}

	// ValidateTask runs the same check, so ax apply rejects bad limits up front.
	meta := &v1alpha1.ObjectMeta{Name: "task", Atespace: "default"}
	err := apivalidation.ValidateTask(&v1alpha1.Task{Metadata: meta, Spec: &v1alpha1.TaskSpec{Resources: limits("abc", "")}})
	if err == nil || !strings.Contains(err.Error(), "spec.resources.limits.cpu") {
		t.Fatalf("ValidateTask error = %v, want cpu limit error", err)
	}
	if err := apivalidation.ValidateTask(&v1alpha1.Task{Metadata: meta, Spec: &v1alpha1.TaskSpec{Resources: limits("2", "4Gi")}}); err != nil {
		t.Fatalf("ValidateTask rejected valid limits: %v", err)
	}
}
