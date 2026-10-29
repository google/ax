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
			Name:       "full",
			Atespace:   "default",
			CreateTime: timestamppb.New(time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)),
		},
		Spec: &v1alpha1.TaskSpec{
			Image:   "example.com/img:1",
			Command: []string{"sh", "-c", "true"},
			Env:     []*v1alpha1.EnvVar{{Name: "A", Value: "1"}},
			Resources: &v1alpha1.ResourcesSpec{
				Requests: &v1alpha1.ResourceList{Cpu: "500m", Memory: "1Gi"},
			},
			Workspace: &v1alpha1.WorkspaceRef{Name: "ws", Path: "/workspace"},
			Debug:     true,
		},
		Status: &v1alpha1.TaskStatus{
			Phase:    "Running",
			Id:       "id-1",
			Actor:    "actor-1",
			WorkerIp: "10.0.0.5:80",
			Conditions: []*v1alpha1.Condition{{
				Type:       "Ready",
				Status:     "True",
				Reason:     "TaskRunning",
				Message:    "ok",
				UpdateTime: timestamppb.New(time.Date(2026, 9, 19, 12, 5, 0, 0, time.UTC)),
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
	for _, want := range []string{"workerIP: 10.0.0.5:80", "updateTime: 2026-09-19T12:05:00Z", "createTime: 2026-09-19T12:00:00Z"} {
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

func TestTaskSpec_WorkspacePath(t *testing.T) {
	tests := []struct {
		name string
		spec *v1alpha1.TaskSpec
		want string
	}{
		{name: "nil spec", want: v1alpha1.DefaultWorkspacePath},
		{name: "no workspace", spec: &v1alpha1.TaskSpec{}, want: v1alpha1.DefaultWorkspacePath},
		{name: "default path", spec: &v1alpha1.TaskSpec{Workspace: &v1alpha1.WorkspaceRef{Name: "one"}}, want: v1alpha1.DefaultWorkspacePath},
		{name: "explicit path", spec: &v1alpha1.TaskSpec{Workspace: &v1alpha1.WorkspaceRef{Name: "one", Path: "/opt/code"}}, want: "/opt/code"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.spec.WorkspacePath(); got != tt.want {
				t.Errorf("WorkspacePath() = %q, want %q", got, tt.want)
			}
		})
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
		{name: "no workspace", spec: &v1alpha1.TaskSpec{}},
		{name: "default path", spec: &v1alpha1.TaskSpec{Workspace: &v1alpha1.WorkspaceRef{Name: "a"}}},
		{name: "explicit path", spec: &v1alpha1.TaskSpec{Workspace: &v1alpha1.WorkspaceRef{Name: "a", Path: "/x"}}},
		{
			name:    "nameless binding",
			spec:    &v1alpha1.TaskSpec{Workspace: &v1alpha1.WorkspaceRef{Path: "/w"}},
			wantErr: "spec.workspace: name is required",
		},
		{
			name:    "relative path",
			spec:    &v1alpha1.TaskSpec{Workspace: &v1alpha1.WorkspaceRef{Name: "a", Path: "rel"}},
			wantErr: "spec.workspace: path \"rel\" must be absolute",
		},
		{
			name: "config",
			spec: &v1alpha1.TaskSpec{Config: mustStruct(t, map[string]any{"suite": "swe", "shard": map[string]any{"index": 7}})},
		},
		{
			name:    "oversized config",
			spec:    &v1alpha1.TaskSpec{Config: mustStruct(t, map[string]any{"blob": strings.Repeat("x", v1alpha1.MaxTaskConfigSize)})},
			wantErr: "spec.config:",
		},
		{
			name: "config near the limit",
			spec: &v1alpha1.TaskSpec{Config: mustStruct(t, map[string]any{"blob": strings.Repeat("x", v1alpha1.MaxTaskConfigSize-100)})},
		},
		{
			// Each value fits on its own, but together they do not fit in one
			// Substrate environment variable.
			name: "oversized task",
			spec: &v1alpha1.TaskSpec{
				Config: mustStruct(t, map[string]any{"blob": strings.Repeat("x", v1alpha1.MaxTaskConfigSize-100)}),
				Env:    []*v1alpha1.EnvVar{{Name: "BIG", Value: strings.Repeat("y", 16<<10)}},
			},
			wantErr: "task: encoded launch configuration",
		},
		{
			name:    "oversized command",
			spec:    &v1alpha1.TaskSpec{Command: []string{"sh", "-c", strings.Repeat("z", v1alpha1.MaxEnvValueSize)}},
			wantErr: "task: encoded launch configuration",
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

const configTaskYAML = `
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: swe-bench-shard-07
spec:
  image: us-docker.pkg.dev/acme/evals/harness:2026.10
  command: ["python3", "-m", "harness.run"]
  config:
    suite: swe_bench_verified
    shard: { index: 7, total: 32 }
    models:
      - provider: google
        model: gemini-3.8-flash
      - provider: google
        model: gemini-3.8-pro
    limits:
      per_case_timeout: 15m
      max_cost_usd: 25.5
    report:
      bucket: gs://acme-evals/results/
      dry_run: true
      since: "2026-10-01T00:00:00Z"
      version: "1"
      owner: null
`

func TestTask_Config_YAML(t *testing.T) {
	var task v1alpha1.Task
	if err := yaml.Unmarshal([]byte(configTaskYAML), &task); err != nil {
		t.Fatal(err)
	}
	got := task.GetSpec().GetConfig().AsMap()
	want := map[string]any{
		"suite": "swe_bench_verified",
		"shard": map[string]any{"index": 7.0, "total": 32.0},
		"models": []any{
			map[string]any{"provider": "google", "model": "gemini-3.8-flash"},
			map[string]any{"provider": "google", "model": "gemini-3.8-pro"},
		},
		"limits": map[string]any{"per_case_timeout": "15m", "max_cost_usd": 25.5},
		"report": map[string]any{
			"bucket":  "gs://acme-evals/results/",
			"dry_run": true,
			"since":   "2026-10-01T00:00:00Z",
			"version": "1",
			"owner":   nil,
		},
	}
	if !proto.Equal(task.GetSpec().GetConfig(), mustStruct(t, want)) {
		t.Fatalf("config = %v, want %v", got, want)
	}

	// The config survives the YAML round trip the controller performs when it
	// builds AX_TASK_YAML, and runners decoding it leniently see it unchanged.
	out, err := yaml.Marshal(&task)
	if err != nil {
		t.Fatal(err)
	}
	var strict v1alpha1.Task
	if err := yaml.Unmarshal(out, &strict); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if !proto.Equal(&task, &strict) {
		t.Errorf("yaml round trip changed the task\n%s", out)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(out, &node); err != nil {
		t.Fatal(err)
	}
	var lenient v1alpha1.Task
	if err := v1alpha1.UnmarshalYAMLLenient(&node, &lenient); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(&task, &lenient) {
		t.Errorf("lenient decoding changed the task\n%s", out)
	}
}

func TestTask_Config_MustBeObject(t *testing.T) {
	for _, config := range []string{"[1, 2]", "hello", "3"} {
		var task v1alpha1.Task
		err := yaml.Unmarshal([]byte("kind: Task\nspec:\n  config: "+config+"\n"), &task)
		if err == nil {
			t.Errorf("config %s: expected an error, got none", config)
		}
	}
}

func TestTask_Workspace_YAML(t *testing.T) {
	taskYAML := `
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: single
spec:
  workspace:
    name: code
    path: /opt/code
`
	var task v1alpha1.Task
	if err := yaml.Unmarshal([]byte(taskYAML), &task); err != nil {
		t.Fatalf("unmarshaling task: %v", err)
	}
	ref := task.Spec.GetWorkspace()
	if ref == nil || ref.Name != "code" || ref.Path != "/opt/code" {
		t.Fatalf("unexpected workspace binding: %v", ref)
	}
	out, err := yaml.Marshal(&task)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "workspace:") || strings.Contains(string(out), "workspaces:") {
		t.Errorf("expected a singular workspace mapping in output:\n%s", out)
	}
	var back v1alpha1.Task
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if !proto.Equal(&task, &back) {
		t.Errorf("round trip changed the task:\n%s", out)
	}

	// The earlier spec.workspaces list is no longer part of the schema, so
	// manifests still using it are rejected like any unknown field.
	listYAML := `
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: multi
spec:
  workspaces:
    - name: code
`
	var rejected v1alpha1.Task
	if err := yaml.Unmarshal([]byte(listYAML), &rejected); err == nil {
		t.Error("expected spec.workspaces list to be rejected")
	}
}

func TestWorkspace_Files_YAML(t *testing.T) {
	wsYAML := `
apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: files-demo
spec:
  files:
    - path: AGENTS.md
      content: "# Agent Guidelines\nFollow best practices."
    - path: README.md
      content: "# Demo Workspace\nWelcome!"
`
	var ws v1alpha1.Workspace
	if err := yaml.Unmarshal([]byte(wsYAML), &ws); err != nil {
		t.Fatalf("unmarshaling workspace: %v", err)
	}
	if len(ws.GetSpec().GetFiles()) != 2 {
		t.Fatalf("expected 2 files, got %d", len(ws.GetSpec().GetFiles()))
	}
	if ws.GetSpec().GetFiles()[0].GetPath() != "AGENTS.md" || ws.GetSpec().GetFiles()[0].GetContent() != "# Agent Guidelines\nFollow best practices." {
		t.Errorf("unexpected file 0: %+v", ws.GetSpec().GetFiles()[0])
	}
	if ws.GetSpec().GetFiles()[1].GetPath() != "README.md" {
		t.Errorf("unexpected file 1: %+v", ws.GetSpec().GetFiles()[1])
	}

	out, err := yaml.Marshal(&ws)
	if err != nil {
		t.Fatal(err)
	}

	var back v1alpha1.Workspace
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if !proto.Equal(&ws, &back) {
		t.Errorf("round trip changed workspace:\n%s", out)
	}
}

func TestValidateWorkspace_Files(t *testing.T) {
	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "valid-ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Files: []*v1alpha1.File{
				{Path: "", Content: "missing path"},
			},
		},
	}
	if err := v1alpha1.ValidateWorkspace(ws); err == nil || !strings.Contains(err.Error(), "spec.files[0]: path is required") {
		t.Errorf("expected path is required error, got %v", err)
	}
}

func TestUnmarshalYAMLLenient_IgnoresUnknownFields(t *testing.T) {
	doc := "metadata:\n  name: t\n  atespace: default\nspec:\n  image: img\n  fieldFromTheFuture: 1\n"
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &node); err != nil {
		t.Fatal(err)
	}

	var strict v1alpha1.Task
	if err := node.Decode(&strict); err == nil {
		t.Fatal("strict decoding should reject an unknown field")
	}

	var lenient v1alpha1.Task
	if err := v1alpha1.UnmarshalYAMLLenient(&node, &lenient); err != nil {
		t.Fatalf("lenient decoding failed: %v", err)
	}
	if lenient.GetSpec().GetImage() != "img" {
		t.Errorf("image = %q, want img", lenient.GetSpec().GetImage())
	}
}

func TestUnmarshalYAMLLenient_NormalizesWorkspace(t *testing.T) {
	doc := "metadata:\n  name: ws\nspec:\n  fieldFromTheFuture: true\n  mcp:\n  - name: srv\n"
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &node); err != nil {
		t.Fatal(err)
	}
	var ws v1alpha1.Workspace
	if err := v1alpha1.UnmarshalYAMLLenient(&node, &ws); err != nil {
		t.Fatalf("lenient decoding failed: %v", err)
	}
	if got := ws.GetSpec().GetMcp().GetServers(); len(got) != 1 || got[0].GetName() != "srv" {
		t.Errorf("mcp servers = %v, want one named srv", got)
	}
}

func TestValidateTask_EgressAndSecretRefs(t *testing.T) {
	inject := func(host, header string) *v1alpha1.CredentialInjection {
		return &v1alpha1.CredentialInjection{Host: host, Header: header, Prefix: "Bearer "}
	}
	secretEnv := func(name string, ref *v1alpha1.SecretEnvSource) *v1alpha1.EnvVar {
		return &v1alpha1.EnvVar{Name: name, SecretRef: ref}
	}
	tests := []struct {
		name    string
		spec    *v1alpha1.TaskSpec
		wantErr string
	}{
		{name: "egress hosts", spec: &v1alpha1.TaskSpec{Egress: &v1alpha1.EgressSpec{Hosts: []string{"pypi.org", "*.googleapis.com"}}}},
		{
			name:    "egress host with scheme",
			spec:    &v1alpha1.TaskSpec{Egress: &v1alpha1.EgressSpec{Hosts: []string{"https://pypi.org"}}},
			wantErr: "spec.egress.hosts[0]",
		},
		{
			name:    "egress bare wildcard",
			spec:    &v1alpha1.TaskSpec{Egress: &v1alpha1.EgressSpec{Hosts: []string{"*"}}},
			wantErr: "spec.egress.hosts[0]",
		},
		{
			name: "valid secret ref",
			spec: &v1alpha1.TaskSpec{Env: []*v1alpha1.EnvVar{
				secretEnv("STRIPE_API_KEY", &v1alpha1.SecretEnvSource{Name: "stripe", Key: "api-key", Inject: inject("api.stripe.com", "Authorization")}),
			}},
		},
		{
			name: "value and secret ref",
			spec: &v1alpha1.TaskSpec{Env: []*v1alpha1.EnvVar{
				{Name: "K", Value: "literal", SecretRef: &v1alpha1.SecretEnvSource{Name: "s", Key: "k", Inject: inject("api.example.com", "Authorization")}},
			}},
			wantErr: "mutually exclusive",
		},
		{
			name:    "secret ref without key",
			spec:    &v1alpha1.TaskSpec{Env: []*v1alpha1.EnvVar{secretEnv("K", &v1alpha1.SecretEnvSource{Name: "s", Inject: inject("api.example.com", "Authorization")})}},
			wantErr: "name and key are required",
		},
		{
			name:    "secret name with slash",
			spec:    &v1alpha1.TaskSpec{Env: []*v1alpha1.EnvVar{secretEnv("K", &v1alpha1.SecretEnvSource{Name: "a/b", Key: "k", Inject: inject("api.example.com", "Authorization")})}},
			wantErr: "secretRef.name",
		},
		{
			name:    "secret key with slash",
			spec:    &v1alpha1.TaskSpec{Env: []*v1alpha1.EnvVar{secretEnv("K", &v1alpha1.SecretEnvSource{Name: "s", Key: "../k", Inject: inject("api.example.com", "Authorization")})}},
			wantErr: "secretRef.key",
		},
		{
			name: "secret key with dots and underscores",
			spec: &v1alpha1.TaskSpec{Env: []*v1alpha1.EnvVar{secretEnv("K", &v1alpha1.SecretEnvSource{Name: "my.secret", Key: "API_KEY.v2", Inject: inject("api.example.com", "Authorization")})}},
		},
		{
			name:    "secret ref without inject",
			spec:    &v1alpha1.TaskSpec{Env: []*v1alpha1.EnvVar{secretEnv("K", &v1alpha1.SecretEnvSource{Name: "s", Key: "k"})}},
			wantErr: "host and header are required",
		},
		{
			name:    "wildcard inject host",
			spec:    &v1alpha1.TaskSpec{Env: []*v1alpha1.EnvVar{secretEnv("K", &v1alpha1.SecretEnvSource{Name: "s", Key: "k", Inject: inject("*.example.com", "Authorization")})}},
			wantErr: "inject.host",
		},
		{
			name:    "invalid header",
			spec:    &v1alpha1.TaskSpec{Env: []*v1alpha1.EnvVar{secretEnv("K", &v1alpha1.SecretEnvSource{Name: "s", Key: "k", Inject: inject("api.example.com", "Bad Header")})}},
			wantErr: "not a valid request header name",
		},
		{
			name:    "host header",
			spec:    &v1alpha1.TaskSpec{Env: []*v1alpha1.EnvVar{secretEnv("K", &v1alpha1.SecretEnvSource{Name: "s", Key: "k", Inject: inject("api.example.com", "Host")})}},
			wantErr: "not a valid request header name",
		},
		{
			name:    "invalid store name",
			spec:    &v1alpha1.TaskSpec{Env: []*v1alpha1.EnvVar{secretEnv("K", &v1alpha1.SecretEnvSource{Store: "Bad_Store", Name: "s", Key: "k", Inject: inject("api.example.com", "Authorization")})}},
			wantErr: "secretRef.store",
		},
		{
			name: "same header injected twice for a host",
			spec: &v1alpha1.TaskSpec{Env: []*v1alpha1.EnvVar{
				secretEnv("A", &v1alpha1.SecretEnvSource{Name: "a", Key: "k", Inject: inject("api.example.com", "Authorization")}),
				secretEnv("B", &v1alpha1.SecretEnvSource{Name: "b", Key: "k", Inject: inject("api.example.com", "authorization")}),
			}},
			wantErr: `already injected by env "A"`,
		},
		{
			name: "different headers for one host",
			spec: &v1alpha1.TaskSpec{Env: []*v1alpha1.EnvVar{
				secretEnv("A", &v1alpha1.SecretEnvSource{Name: "a", Key: "k", Inject: inject("api.example.com", "Authorization")}),
				secretEnv("B", &v1alpha1.SecretEnvSource{Name: "b", Key: "k", Inject: inject("api.example.com", "X-Api-Key")}),
			}},
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

func TestEnvVar_ResolvedValue(t *testing.T) {
	if got := (&v1alpha1.EnvVar{Name: "A", Value: "v"}).ResolvedValue(); got != "v" {
		t.Errorf("literal: got %q", got)
	}
	ref := &v1alpha1.EnvVar{Name: "B", SecretRef: &v1alpha1.SecretEnvSource{Name: "s", Key: "k"}}
	if got := ref.ResolvedValue(); got != v1alpha1.SecretPlaceholder {
		t.Errorf("secret ref: got %q, want placeholder", got)
	}
}

func TestSecretStore_YAML(t *testing.T) {
	in := `apiVersion: ax.io/v1alpha1
kind: SecretStore
metadata:
  name: default
  atespace: team-a
spec:
  kubernetes:
    namespace: team-a-secrets
`
	var s v1alpha1.SecretStore
	if err := yaml.Unmarshal([]byte(in), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := s.GetSpec().GetKubernetes().GetNamespace(); got != "team-a-secrets" {
		t.Errorf("namespace = %q", got)
	}
	if err := v1alpha1.ValidateSecretStore(&s); err != nil {
		t.Errorf("ValidateSecretStore: %v", err)
	}
	if err := v1alpha1.ValidateSecretStore(&v1alpha1.SecretStore{Metadata: &v1alpha1.ObjectMeta{Name: "x"}, Spec: &v1alpha1.SecretStoreSpec{}}); err == nil {
		t.Error("expected an error for a store without a backend")
	}
}

func TestTask_SecretRef_YAML(t *testing.T) {
	in := `apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: t
spec:
  egress:
    hosts: [pypi.org]
  env:
    - name: STRIPE_API_KEY
      secretRef:
        name: stripe
        key: api-key
        inject:
          host: api.stripe.com
          header: Authorization
          prefix: "Bearer "
`
	var task v1alpha1.Task
	if err := yaml.Unmarshal([]byte(in), &task); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ref := task.GetSpec().GetEnv()[0].GetSecretRef()
	if ref.StoreName() != v1alpha1.DefaultSecretStore || ref.GetInject().GetHost() != "api.stripe.com" || ref.GetInject().GetPrefix() != "Bearer " {
		t.Errorf("unexpected secretRef: %v", ref)
	}
	if err := v1alpha1.ValidateTask(&task); err != nil {
		t.Errorf("ValidateTask: %v", err)
	}
}
