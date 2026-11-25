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
	"context"
	"strings"
	"testing"

	"github.com/google/ax/internal/apivalidation"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"k8s.io/apimachinery/pkg/api/operation"
	"k8s.io/apimachinery/pkg/util/validation/field"
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
	invalid := func(name string) field.ErrorList {
		return field.ErrorList{field.Invalid(field.NewPath(name), nil, "").WithOrigin("format=k8s-short-name")}
	}
	tests := []struct {
		name string
		meta *v1alpha1.ObjectMeta
		want field.ErrorList
	}{
		{name: "missing name", meta: &v1alpha1.ObjectMeta{Atespace: "default"}, want: field.ErrorList{field.Required(field.NewPath("name"), "")}},
		{name: "name only", meta: &v1alpha1.ObjectMeta{Name: "task123"}},
		{name: "name and atespace", meta: &v1alpha1.ObjectMeta{Name: "task123", Atespace: "team-a"}},
		{name: "uppercase name", meta: &v1alpha1.ObjectMeta{Name: "Task-With-Caps"}, want: invalid("name")},
		{name: "underscore in name", meta: &v1alpha1.ObjectMeta{Name: "my_task"}, want: invalid("name")},
		{name: "name too long", meta: &v1alpha1.ObjectMeta{Name: strings.Repeat("x", 64)}, want: invalid("name")},
		{name: "uppercase atespace", meta: &v1alpha1.ObjectMeta{Name: "task123", Atespace: "Default"}, want: invalid("atespace")},
		{name: "dotted atespace", meta: &v1alpha1.ObjectMeta{Name: "task123", Atespace: "team.a"}, want: invalid("atespace")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op := operation.Operation{Type: operation.Create}
			assertValidateErr(t, apivalidation.Validate_ObjectMeta(context.Background(), op, nil, tt.meta, nil), tt.want)
		})
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
		{name: "cpu zero", reqs: limits("0", ""), wantErr: `spec.profile.resources.limits.cpu: "0" must be greater than zero`},
		{name: "cpu zero millicores", reqs: limits("0m", ""), wantErr: "must be greater than zero"},
		{name: "cpu negative", reqs: limits("-1", ""), wantErr: "must be greater than zero"},
		{name: "cpu at bound", reqs: limits("1000", ""), wantErr: `spec.profile.resources.limits.cpu: "1000" must be less than 1000 cores`},
		{name: "cpu over bound via suffix", reqs: limits("1k", ""), wantErr: "must be less than 1000 cores"},
		{name: "cpu over bound via exponent", reqs: limits("1e3", ""), wantErr: "must be less than 1000 cores"},
		{name: "cpu over bound via binary suffix", reqs: limits("1Ki", ""), wantErr: "must be less than 1000 cores"},
		{name: "cpu not a quantity", reqs: limits("two", ""), wantErr: `spec.profile.resources.limits.cpu: invalid quantity "two"`},
		{name: "cpu bad suffix", reqs: limits("2cores", ""), wantErr: "invalid quantity"},
		{name: "cpu double dot", reqs: limits("1.2.3", ""), wantErr: "invalid quantity"},
		{name: "cpu whitespace", reqs: limits(" 2", ""), wantErr: "invalid quantity"},
		{name: "memory zero", reqs: limits("", "0Gi"), wantErr: `spec.profile.resources.limits.memory: "0Gi" must be greater than zero`},
		{name: "memory negative", reqs: limits("", "-4Gi"), wantErr: "must be greater than zero"},
		{name: "memory lowercase suffix", reqs: limits("", "4gi"), wantErr: `spec.profile.resources.limits.memory: invalid quantity "4gi"`},
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

	// ValidateCreateTaskRequest runs the same check, so ax apply rejects bad limits up front.
	meta := &v1alpha1.ObjectMeta{Name: "task", Atespace: "default"}
	err := validateTask(&v1alpha1.Task{Metadata: meta, Spec: &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Resources: limits("abc", "")}}})
	if err == nil || !strings.Contains(err.Error(), "spec.profile.resources.limits.cpu") {
		t.Fatalf("ValidateTask error = %v, want cpu limit error", err)
	}
	if err := validateTask(&v1alpha1.Task{Metadata: meta, Spec: &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "img@sha256:abc", Resources: limits("2", "4Gi")}}}); err != nil {
		t.Fatalf("ValidateTask rejected valid limits: %v", err)
	}
}

func assertValidateErr(t *testing.T, got field.ErrorList, want field.ErrorList) {
	t.Helper()
	field.ErrorMatcher{}.ByType().ByField().ByOrigin().Test(t, want, got)
}
