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

package store

import (
	"context"
	"errors"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

var (
	ErrNotFound = errors.New("resource not found")
)

// TODO(jbd): Better pagination.

// Store defines the storage interface for AX resources.
type Store interface {
	SaveTask(ctx context.Context, task *v1alpha1.Task) error
	GetTask(ctx context.Context, atespace, name string) (*v1alpha1.Task, error)
	ListTasks(ctx context.Context, atespace string, limit, offset int64) ([]*v1alpha1.Task, error)
	UpdateTaskStatus(ctx context.Context, atespace, name string, status *v1alpha1.TaskStatus) error
	DeleteTask(ctx context.Context, atespace, name string) error

	SaveWorkspace(ctx context.Context, ws *v1alpha1.Workspace) error
	GetWorkspace(ctx context.Context, atespace, name string) (*v1alpha1.Workspace, error)
	ListWorkspaces(ctx context.Context, atespace string, limit, offset int64) ([]*v1alpha1.Workspace, error)
	UpdateWorkspaceStatus(ctx context.Context, atespace, name string, status *v1alpha1.WorkspaceStatus) error
	DeleteWorkspace(ctx context.Context, atespace, name string) error

	SaveGateway(ctx context.Context, gw *v1alpha1.Gateway) error
	GetGateway(ctx context.Context, atespace, name string) (*v1alpha1.Gateway, error)
	ListGateways(ctx context.Context, atespace string) ([]*v1alpha1.Gateway, error)
	DeleteGateway(ctx context.Context, atespace, name string) error

	SaveModel(ctx context.Context, model *v1alpha1.Model) error
	GetModel(ctx context.Context, atespace, name string) (*v1alpha1.Model, error)
	ListModels(ctx context.Context, atespace string) ([]*v1alpha1.Model, error)
	DeleteModel(ctx context.Context, atespace, name string) error

	Close() error
}

// WorkingQueue holds the working tasks, those whose end ax-server has yet to
// record: ax-server asks their runners how they ended. It is kept by the
// Store that holds the tasks, so that deleting a task removes it from the
// queue.
type WorkingQueue interface {
	// Queue adds a stored task to the queue. Queueing a task again is not an
	// error. It returns ErrNotFound if the task is not stored.
	Queue(ctx context.Context, task *v1alpha1.ObjectRef) error
	// Claim returns the queued tasks of all atespaces. The tasks stay queued.
	Claim(ctx context.Context) ([]*v1alpha1.ObjectRef, error)
	// Delete removes a task from the queue. Deleting a task that is not
	// queued is not an error.
	Delete(ctx context.Context, task *v1alpha1.ObjectRef) error
}
