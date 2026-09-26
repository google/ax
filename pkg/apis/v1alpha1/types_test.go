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

package v1alpha1_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"
)

// fullTask returns a Task with every field populated so round trips exercise
// the whole schema, including timestamps and status.
func fullTask() *v1alpha1.Task {
	return &v1alpha1.Task{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindTask,
		Metadata: &v1alpha1.ObjectMeta{
			Name:              "full",
			Atespace:          "default",
			CreationTimestamp: timestamppb.New(time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)),
		},
		Spec: &v1alpha1.TaskSpec{
			Image:   "example.com/img:1",
			Command: []string{"sh", "-c", "true"},
			Env:     []*v1alpha1.EnvVar{{Name: "A", Value: "1"}},
			Resources: &v1alpha1.ResourceReqs{
				Requests: &v1alpha1.ResourceList{Cpu: "500m", Memory: "1Gi"},
			},
			Workspaces: []*v1alpha1.WorkspaceRef{{Name: "ws", Path: "/workspace", Goal: "build"}},
			Debug:      true,
		},
		Status: &v1alpha1.TaskStatus{
			Phase:    "Running",
			Id:       "id-1",
			Actor:    "actor-1",
			WorkerIp: "10.0.0.5:80",
			Conditions: []*v1alpha1.Condition{{
				Type:               "Ready",
				Status:             "True",
				Reason:             "TaskRunning",
				Message:            "ok",
				LastTransitionTime: timestamppb.New(time.Date(2026, 9, 19, 12, 5, 0, 0, time.UTC)),
			}},
		},
	}
}

func TestTask_RoundTrip(t *testing.T) {
	want := fullTask()
	out, err := yaml.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.Task
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if !proto.Equal(want, &got) {
		t.Errorf("yaml round trip changed the task\n%s", out)
	}
}

func TestTask_YAMLFieldNames(t *testing.T) {
	out, err := yaml.Marshal(fullTask())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"workerIP: 10.0.0.5:80", "lastTransitionTime: 2026-09-19T12:05:00Z", "creationTimestamp: 2026-09-19T12:00:00Z"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("yaml missing %s:\n%s", want, out)
		}
	}
}

func TestTask_YAMLOutputShape(t *testing.T) {
	out, err := yaml.Marshal(fullTask())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)

	// Top-level keys follow the proto definition, not alphabetical order.
	order := []string{"apiVersion:", "kind:", "metadata:", "spec:", "status:"}
	last := -1
	for _, key := range order {
		idx := strings.Index(s, "\n"+key)
		if key == "apiVersion:" {
			idx = strings.Index(s, key)
		}
		if idx < 0 || idx < last {
			t.Errorf("expected %s in proto order, got:\n%s", key, s)
		}
		last = idx
	}

	// Strings that look like other types are quoted so they read back as strings.
	if !strings.Contains(s, `status: "True"`) {
		t.Errorf("expected condition status to be quoted:\n%s", s)
	}
	// Unset fields are omitted rather than rendered as null.
	if strings.Contains(s, "null") {
		t.Errorf("expected no null values:\n%s", s)
	}
}

func TestStrictDecoding_RejectsUnknownFields(t *testing.T) {
	var task v1alpha1.Task
	err := yaml.Unmarshal([]byte("kind: Task\nspec:\n  imgae: typo\n"), &task)
	if err == nil || !strings.Contains(err.Error(), "imgae") {
		t.Errorf("expected error naming the unknown field, got %v", err)
	}
}

func TestWorkspace_MCP_UnmarshalYAML(t *testing.T) {
	// Legacy: spec.mcp as a bare list of servers.
	flatYAML := `
kind: Workspace
spec:
  mcp:
    - name: tool-1
      endpoint: "http://tool-1:8080"
    - name: tool-2
      command: "npx"
      args: ["-y", "@modelcontextprotocol/server"]
`
	var flatWS v1alpha1.Workspace
	if err := yaml.Unmarshal([]byte(flatYAML), &flatWS); err != nil {
		t.Fatalf("unmarshaling flat mcp: %v", err)
	}
	if len(flatWS.Spec.Mcp.GetServers()) != 2 || flatWS.Spec.Mcp.Servers[0].Name != "tool-1" {
		t.Fatalf("unexpected servers from flat list: %+v", flatWS.Spec.Mcp)
	}

	// Current: registries and servers, including a static registry with nested servers.
	structuredYAML := `
kind: Workspace
spec:
  mcp:
    registries:
      - provider: google
        query: "mcp.tags:git"
      - provider: static
        servers:
          - name: git-tools
            endpoint: "http://git-mcp.default.svc.cluster.local:8080"
    servers:
      - name: tool-1
        endpoint: "http://tool-1:8080"
`
	var ws v1alpha1.Workspace
	if err := yaml.Unmarshal([]byte(structuredYAML), &ws); err != nil {
		t.Fatalf("unmarshaling structured mcp: %v", err)
	}
	if len(ws.Spec.Mcp.Registries) != 2 || ws.Spec.Mcp.Registries[0].Query != "mcp.tags:git" {
		t.Errorf("unexpected registries: %+v", ws.Spec.Mcp.Registries)
	}
	if got := ws.Spec.Mcp.Registries[1]; got.Provider != "static" || len(got.Servers) != 1 || got.Servers[0].Name != "git-tools" {
		t.Errorf("unexpected static registry: %+v", got)
	}
	if len(ws.Spec.Mcp.Servers) != 1 {
		t.Errorf("unexpected servers: %+v", ws.Spec.Mcp.Servers)
	}
}

func TestWorkspace_RoundTrip(t *testing.T) {
	wsYAML := `
apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: full-ws
  atespace: default
spec:
  git:
    - name: origin
      repo: "https://github.com/chalk/chalk.git"
      branch: "main"
      dir: chalk
      depth: 1
  mcp:
    registries:
      - provider: google
        query: "mcp.tags:build"
  skills:
    registries:
      - provider: google
        query: "skills.tags:golang"
    path: "/.agents/skills"
`
	var ws v1alpha1.Workspace
	if err := yaml.Unmarshal([]byte(wsYAML), &ws); err != nil {
		t.Fatalf("unmarshaling workspace: %v", err)
	}
	if ws.Spec.Git[0].Depth != 1 || ws.Spec.Skills.Path != "/.agents/skills" {
		t.Fatalf("unexpected workspace: %+v", ws.Spec)
	}

	out, err := yaml.Marshal(&ws)
	if err != nil {
		t.Fatal(err)
	}
	var again v1alpha1.Workspace
	if err := yaml.Unmarshal(out, &again); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(&ws, &again) {
		t.Errorf("workspace round trip changed:\n%s", out)
	}
}

// The original schema bound a single workspace under spec.workspace. That field
// is gone, so manifests still using it are rejected like any unknown field.
func TestTask_SingularWorkspace_Rejected(t *testing.T) {
	taskYAML := `
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: test-task
spec:
  workspace:
    name: default-workspace
`
	var task v1alpha1.Task
	err := yaml.Unmarshal([]byte(taskYAML), &task)
	if err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("expected spec.workspace to be rejected as unknown, got %v", err)
	}
}

func TestModel_SecretKey_UnmarshalYAML(t *testing.T) {
	cases := map[string]string{
		"structured": `
spec:
  provider: google
  secretKey:
    name: the-secret
    key: the-key
`,
		"legacy secretKeyRef": `
spec:
  provider: google
  secretKeyRef:
    name: the-secret
    key: the-key
`,
		"legacy apiKey.secretKeyRef": `
spec:
  provider: google
  apiKey:
    secretKeyRef:
      name: the-secret
      key: the-key
`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			var m v1alpha1.Model
			if err := yaml.Unmarshal([]byte("kind: Model\n"+src), &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := m.Spec.GetSecretKey(); got.GetName() != "the-secret" || got.GetKey() != "the-key" {
				t.Errorf("unexpected secretKey: %+v", got)
			}
		})
	}

	t.Run("legacy bare string", func(t *testing.T) {
		var m v1alpha1.Model
		if err := yaml.Unmarshal([]byte("kind: Model\nspec:\n  provider: google\n  secretKey: GEMINI_API_KEY\n"), &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got := m.Spec.GetSecretKey(); got.GetName() != "GEMINI_API_KEY" || got.GetKey() != "GEMINI_API_KEY" {
			t.Errorf("unexpected secretKey: %+v", got)
		}
	})
}

func TestModel_RoundTrip(t *testing.T) {
	want := &v1alpha1.Model{
		ApiVersion: v1alpha1.APIVersion,
		Kind:       v1alpha1.KindModel,
		Metadata:   &v1alpha1.ObjectMeta{Name: "m", Atespace: "default"},
		Spec: &v1alpha1.ModelSpec{
			Provider:  "google",
			Model:     "gemini-3.8-flash",
			SecretKey: &v1alpha1.SecretKeyRef{Name: "gemini-api-secret", Key: "GEMINI_API_KEY"},
			Parameters: mustStruct(t, map[string]any{
				"temperature":       0.2,
				"maxOutputTokens":   4096,
				"systemInstruction": "be brief",
				"stopSequences":     []any{"END"},
			}),
		},
	}
	out, err := yaml.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.Model
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if !proto.Equal(want, &got) {
		t.Errorf("model round trip changed:\n%s", out)
	}
	for _, want := range []string{"temperature: 0.2", "maxOutputTokens: 4096", "- END"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("expected %q in parameters output:\n%s", want, out)
		}
	}
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTaskSpec_WorkspaceRefs(t *testing.T) {
	single := &v1alpha1.TaskSpec{Workspaces: []*v1alpha1.WorkspaceRef{{Name: "one"}}}
	if refs := single.WorkspaceRefs(); len(refs) != 1 || refs[0].Name != "one" {
		t.Errorf("single: refs = %v", refs)
	}
	if paths := single.WorkspacePaths(); len(paths) != 1 || paths[0] != v1alpha1.DefaultWorkspacePath+"/one" {
		t.Errorf("single: paths = %v, want [%s/one]", paths, v1alpha1.DefaultWorkspacePath)
	}

	multi := &v1alpha1.TaskSpec{Workspaces: []*v1alpha1.WorkspaceRef{
		{Name: "code"},
		nil,
		{Name: "tools", Path: "/opt/tools"},
	}}
	refs := multi.WorkspaceRefs()
	if len(refs) != 2 || refs[0].Name != "code" || refs[1].Name != "tools" {
		t.Errorf("multi: refs = %v", refs)
	}
	if paths := multi.WorkspacePaths(); len(paths) != 2 || paths[0] != "/workspace/code" || paths[1] != "/opt/tools" {
		t.Errorf("multi: paths = %v", paths)
	}

	var nilSpec *v1alpha1.TaskSpec
	if refs := nilSpec.WorkspaceRefs(); len(refs) != 0 {
		t.Errorf("nil spec: refs = %v", refs)
	}
	if paths := (&v1alpha1.TaskSpec{}).WorkspacePaths(); len(paths) != 0 {
		t.Errorf("empty spec: paths = %v", paths)
	}
}

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
		if err := v1alpha1.ValidateName(name); err != nil {
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
		if err := v1alpha1.ValidateName(name); err == nil {
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
			err := v1alpha1.ValidateObjectMeta(tt.meta)
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
	if err := v1alpha1.ValidateTask(&v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: "Bad"}}); err == nil {
		t.Error("ValidateTask accepted an invalid name")
	}
	if err := v1alpha1.ValidateWorkspace(&v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "Bad"}}); err == nil {
		t.Error("ValidateWorkspace accepted an invalid name")
	}
	if err := v1alpha1.ValidateModel(&v1alpha1.Model{Metadata: &v1alpha1.ObjectMeta{Name: "Bad"}}); err == nil {
		t.Error("ValidateModel accepted an invalid name")
	}
}

func TestValidateTask(t *testing.T) {
	tests := []struct {
		name    string
		spec    *v1alpha1.TaskSpec
		wantErr string
	}{
		{name: "nil spec"},
		{name: "no workspaces", spec: &v1alpha1.TaskSpec{}},
		{name: "list of one", spec: &v1alpha1.TaskSpec{Workspaces: []*v1alpha1.WorkspaceRef{{Name: "a"}}}},
		{
			name:    "nameless entry with path",
			spec:    &v1alpha1.TaskSpec{Workspaces: []*v1alpha1.WorkspaceRef{{Path: "/w"}}},
			wantErr: "spec.workspaces[0]: name is required",
		},
		{name: "list default paths", spec: &v1alpha1.TaskSpec{Workspaces: []*v1alpha1.WorkspaceRef{{Name: "a"}, {Name: "b"}}}},
		{name: "list explicit paths", spec: &v1alpha1.TaskSpec{Workspaces: []*v1alpha1.WorkspaceRef{{Name: "a", Path: "/x"}, {Name: "b", Path: "/y"}}}},
		{
			name:    "nameless entry in list",
			spec:    &v1alpha1.TaskSpec{Workspaces: []*v1alpha1.WorkspaceRef{{Name: "a"}, {Path: "/b"}}},
			wantErr: "spec.workspaces[1]: name is required",
		},
		{
			name:    "duplicate name",
			spec:    &v1alpha1.TaskSpec{Workspaces: []*v1alpha1.WorkspaceRef{{Name: "a", Path: "/x"}, {Name: "a", Path: "/y"}}},
			wantErr: `workspace "a" is bound more than once`,
		},
		{
			name:    "duplicate path",
			spec:    &v1alpha1.TaskSpec{Workspaces: []*v1alpha1.WorkspaceRef{{Name: "a", Path: "/same"}, {Name: "b", Path: "/same/"}}},
			wantErr: `path "/same/" is already used by workspace "a"`,
		},
		{
			name:    "default path collides with explicit",
			spec:    &v1alpha1.TaskSpec{Workspaces: []*v1alpha1.WorkspaceRef{{Name: "a"}, {Name: "b", Path: "/workspace/a"}}},
			wantErr: "already used",
		},
		{
			name:    "relative path",
			spec:    &v1alpha1.TaskSpec{Workspaces: []*v1alpha1.WorkspaceRef{{Name: "a", Path: "rel"}}},
			wantErr: "spec.workspaces[0]: path \"rel\" must be absolute",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v1alpha1.ValidateTask(&v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: "task"}, Spec: tt.spec})
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
}

func TestTask_Workspaces_YAML(t *testing.T) {
	taskYAML := `
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: multi
spec:
  workspaces:
    - name: code
      goal: "build it"
    - name: tools
      path: /opt/tools
`
	var task v1alpha1.Task
	if err := yaml.Unmarshal([]byte(taskYAML), &task); err != nil {
		t.Fatalf("unmarshaling task: %v", err)
	}
	refs := task.Spec.WorkspaceRefs()
	if len(refs) != 2 || refs[0].Name != "code" || refs[0].Goal != "build it" || refs[1].Path != "/opt/tools" {
		t.Fatalf("unexpected refs: %v", refs)
	}
	out, err := yaml.Marshal(&task)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "workspaces:") || strings.Contains(string(out), "\n  workspace:") {
		t.Errorf("expected workspaces list and no singular workspace in output:\n%s", out)
	}
	var back v1alpha1.Task
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if !proto.Equal(&task, &back) {
		t.Errorf("round trip changed the task:\n%s", out)
	}
}
