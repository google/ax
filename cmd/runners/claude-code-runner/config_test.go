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
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/types/known/structpb"
)

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("NewStruct: %v", err)
	}
	return s
}

func TestClaudeArgs_Errors(t *testing.T) {
	for name, m := range map[string]map[string]any{
		"missing prompt":     {"model": "claude-opus-5-5"},
		"empty prompt":       {"prompt": ""},
		"non-string prompt":  {"prompt": 1},
		"flag with dashes":   {"prompt": "p", "--model": "x"},
		"kebab-case key":     {"prompt": "p", "max-turns": 5},
		"snake_case key":     {"prompt": "p", "allowed_tools": []any{"Edit"}},
		"PascalCase key":     {"prompt": "p", "MaxTurns": 5},
		"capital run":        {"prompt": "p", "maxTURNS": 5},
		"trailing capital":   {"prompt": "p", "modelX": "x"},
		"flag with equals":   {"prompt": "p", "model=x": "y"},
		"null value":         {"prompt": "p", "model": nil},
		"nested list":        {"prompt": "p", "allowedTools": []any{[]any{"Bash"}}},
		"object in list":     {"prompt": "p", "allowedTools": []any{map[string]any{}}},
		"bool in list":       {"prompt": "p", "allowedTools": []any{true}},
		"empty flag name":    {"prompt": "p", "": "x"},
		"leading digit flag": {"prompt": "p", "1model": "x"},
	} {
		t.Run(name, func(t *testing.T) {
			if args, err := claudeArgs(mustStruct(t, m), nil); err == nil {
				t.Errorf("claudeArgs(%v) = %q, want error", m, args)
			}
		})
	}
}

func TestClaudeArgs_Minimal(t *testing.T) {
	got, err := claudeArgs(mustStruct(t, map[string]any{"prompt": "fix the tests"}), &v1alpha1.MCPConfig{})
	if err != nil {
		t.Fatalf("claudeArgs: %v", err)
	}
	want := []string{"claude", "--print", "--", "fix the tests"}
	if !slices.Equal(got, want) {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestClaudeArgs_Flags(t *testing.T) {
	got, err := claudeArgs(mustStruct(t, map[string]any{
		"prompt":         "-starts with a dash",
		"model":          "claude-opus-5-5",
		"maxTurns":       50,
		"permissionMode": "bypassPermissions",
		"allowedTools":   []any{"Bash(git diff:*)", "Edit"},
		"verbose":        true,
		"debug":          false,
		"settings":       map[string]any{"includeCoAuthoredBy": false},
	}), nil)
	if err != nil {
		t.Fatalf("claudeArgs: %v", err)
	}
	// Flags are emitted in sorted key order.
	want := []string{
		"claude", "--print",
		"--allowed-tools=Bash(git diff:*)", "--allowed-tools=Edit",
		"--max-turns=50",
		"--model=claude-opus-5-5",
		"--permission-mode=bypassPermissions",
		`--settings={"includeCoAuthoredBy":false}`,
		"--verbose",
		"--", "-starts with a dash",
	}
	if !slices.Equal(got, want) {
		t.Errorf("args =\n%q\nwant\n%q", got, want)
	}
}

func TestClaudeArgs_WorkspaceMCP(t *testing.T) {
	mcp := &v1alpha1.MCPConfig{
		Servers: []*v1alpha1.MCPServer{
			{Name: "docs", Endpoint: "https://example.com/mcp"},
			{Name: "git", Command: "git-mcp", Args: []string{"--repo", "."}},
			{Name: "empty"},
		},
		Registries: []*v1alpha1.MCPRegistry{{
			Provider: "google",
			Servers: []*v1alpha1.MCPServer{
				{Name: "docs", Endpoint: "https://shadowed.example.com/mcp"},
				{Name: "build", Endpoint: "https://build.example.com/mcp"},
			},
		}},
	}
	got, err := claudeArgs(mustStruct(t, map[string]any{"prompt": "p"}), mcp)
	if err != nil {
		t.Fatalf("claudeArgs: %v", err)
	}
	var config string
	for _, a := range got {
		if v, ok := strings.CutPrefix(a, "--mcp-config="); ok {
			config = v
		}
	}
	var gotMCP, wantMCP any
	if err := json.Unmarshal([]byte(config), &gotMCP); err != nil {
		t.Fatalf("decoding --mcp-config in %q: %v", got, err)
	}
	_ = json.Unmarshal([]byte(`{"mcpServers": {
		"docs":  {"type": "http", "url": "https://example.com/mcp"},
		"git":   {"type": "stdio", "command": "git-mcp", "args": ["--repo", "."]},
		"build": {"type": "http", "url": "https://build.example.com/mcp"}
	}}`), &wantMCP)
	if !reflect.DeepEqual(gotMCP, wantMCP) {
		t.Errorf("--mcp-config = %s", config)
	}
}

func TestWithClaudeCommand(t *testing.T) {
	config := mustStruct(t, map[string]any{"prompt": "p"})

	explicit := &v1alpha1.Task{Spec: &v1alpha1.TaskSpec{Command: []string{"make", "test"}, Config: config}}
	if got, err := withClaudeCommand(explicit, nil); err != nil || got != explicit {
		t.Errorf("task with spec.command was changed: %v, %v", got, err)
	}

	noConfig := &v1alpha1.Task{Spec: &v1alpha1.TaskSpec{}}
	if got, err := withClaudeCommand(noConfig, nil); err != nil || got != noConfig {
		t.Errorf("task without spec.config was changed: %v, %v", got, err)
	}

	task := &v1alpha1.Task{Spec: &v1alpha1.TaskSpec{Config: config}}
	ws := &v1alpha1.Workspace{Spec: &v1alpha1.WorkspaceSpec{Mcp: &v1alpha1.MCPConfig{
		Servers: []*v1alpha1.MCPServer{{Name: "docs", Endpoint: "https://example.com/mcp"}},
	}}}
	got, err := withClaudeCommand(task, ws)
	if err != nil {
		t.Fatalf("withClaudeCommand: %v", err)
	}
	cmd := got.GetSpec().GetCommand()
	if len(cmd) == 0 || cmd[0] != claudeBinary {
		t.Errorf("command = %q", cmd)
	}
	if !slices.ContainsFunc(cmd, func(a string) bool { return strings.HasPrefix(a, "--mcp-config=") }) {
		t.Errorf("command %q does not load the workspace MCP servers", cmd)
	}
	if len(task.GetSpec().GetCommand()) != 0 {
		t.Errorf("input task was modified")
	}
}
