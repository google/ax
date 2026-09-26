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
	"context"
	"strings"
	"testing"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"
)

type mockAXClient struct {
	v1alpha1.AXClient
	createTaskFn      func(ctx context.Context, in *v1alpha1.CreateTaskRequest, opts ...grpc.CallOption) (*v1alpha1.Task, error)
	getWorkspaceFn    func(ctx context.Context, in *v1alpha1.GetWorkspaceRequest, opts ...grpc.CallOption) (*v1alpha1.Workspace, error)
	updateWorkspaceFn func(ctx context.Context, in *v1alpha1.UpdateWorkspaceRequest, opts ...grpc.CallOption) (*v1alpha1.Workspace, error)
}

func (m *mockAXClient) CreateTask(ctx context.Context, in *v1alpha1.CreateTaskRequest, opts ...grpc.CallOption) (*v1alpha1.Task, error) {
	if m.createTaskFn != nil {
		return m.createTaskFn(ctx, in, opts...)
	}
	return m.AXClient.CreateTask(ctx, in, opts...)
}

func (m *mockAXClient) GetWorkspace(ctx context.Context, in *v1alpha1.GetWorkspaceRequest, opts ...grpc.CallOption) (*v1alpha1.Workspace, error) {
	if m.getWorkspaceFn != nil {
		return m.getWorkspaceFn(ctx, in, opts...)
	}
	return m.AXClient.GetWorkspace(ctx, in, opts...)
}

func (m *mockAXClient) UpdateWorkspace(ctx context.Context, in *v1alpha1.UpdateWorkspaceRequest, opts ...grpc.CallOption) (*v1alpha1.Workspace, error) {
	if m.updateWorkspaceFn != nil {
		return m.updateWorkspaceFn(ctx, in, opts...)
	}
	return m.AXClient.UpdateWorkspace(ctx, in, opts...)
}

func TestApplyDocument_CreateNewTask(t *testing.T) {
	manifestYAML := `
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: my-task
  atespace: default
spec:
  image: ubuntu:latest
  command: ["echo", "hello"]
`
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(manifestYAML), &node); err != nil {
		t.Fatalf("failed to parse YAML: %v", err)
	}

	created := false
	client := &mockAXClient{
		createTaskFn: func(ctx context.Context, in *v1alpha1.CreateTaskRequest, opts ...grpc.CallOption) (*v1alpha1.Task, error) {
			created = true
			return in.Task, nil
		},
	}

	docNode := node.Content[0]
	kind, name, outcome, err := applyDocument(context.Background(), client, docNode)
	if err != nil {
		t.Fatalf("expected applyDocument to succeed, got: %v", err)
	}
	if !created {
		t.Errorf("expected CreateTask to be called")
	}
	if kind != "Task" || name != "my-task" || outcome != "created" {
		t.Errorf("expected Task/my-task created, got %s/%s %s", kind, name, outcome)
	}
}

func TestApplyDocument_ExistingTaskFailsImmutable(t *testing.T) {
	manifestYAML := `
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: existing-task
  atespace: default
spec:
  image: ubuntu:latest
  command: ["echo", "hello"]
`
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(manifestYAML), &node); err != nil {
		t.Fatalf("failed to parse YAML: %v", err)
	}

	client := &mockAXClient{
		createTaskFn: func(ctx context.Context, in *v1alpha1.CreateTaskRequest, opts ...grpc.CallOption) (*v1alpha1.Task, error) {
			return nil, status.Errorf(codes.FailedPrecondition, "task %s/%s already exists and is immutable", in.Task.GetMetadata().GetAtespace(), in.Task.GetMetadata().GetName())
		},
	}

	docNode := node.Content[0]
	_, _, _, err := applyDocument(context.Background(), client, docNode)
	if err == nil {
		t.Fatalf("expected error applying to existing task, got nil")
	}
	if !strings.Contains(err.Error(), "already exists and is immutable") {
		t.Errorf("expected error to mention 'already exists and is immutable', got %v", err)
	}
}
