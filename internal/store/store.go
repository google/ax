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
	"io"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

var (
	ErrNotFound      = errors.New("resource not found")
	ErrAlreadyExists = errors.New("resource already exists")
)

// TaskEvent represents an event published to the task event stream.
type TaskEvent struct {
	ID       string
	Atespace string
	Name     string
	Action   string // "reconcile", "suspend", "resume", "delete"
}

// EventQueue delivers task events to groups of cooperating workers. Every event
// is delivered to exactly one member of a group, and stays pending until that
// member acknowledges it, so a crashed worker's events can be picked up again.
type EventQueue interface {
	// PublishEvent publishes a task event to the stream for worker consumption.
	PublishEvent(ctx context.Context, ev TaskEvent) error
	// Subscribe joins group as consumer, creating the group if it does not exist.
	// All members of a group share one stream of events.
	Subscribe(ctx context.Context, group, consumer string) (Subscription, error)
}

// Subscription is one consumer's view of an EventQueue group.
type Subscription interface {
	// Next blocks until an event is available or ctx is done.
	Next(ctx context.Context) (TaskEvent, error)
	// Ack marks an event as processed so it is not delivered again.
	Ack(ctx context.Context, ev TaskEvent) error
	// Close releases the subscription. Unacknowledged events stay pending for the group.
	Close() error
}

// Store defines the storage and event streaming interface for AX resources.
type Store interface {
	EventQueue

	CreateTask(ctx context.Context, task *v1alpha1.Task) error
	GetTask(ctx context.Context, atespace, name string) (*v1alpha1.Task, error)
	ListTasks(ctx context.Context, atespace string, limit, offset int64) ([]*v1alpha1.Task, error)
	UpdateTaskStatus(ctx context.Context, atespace, name string, status *v1alpha1.TaskStatus) error
	// DeleteTask removes the task record. It publishes no event; callers are
	// expected to have cleaned up the task's actor first.
	DeleteTask(ctx context.Context, atespace, name string) error

	SaveGateway(ctx context.Context, gw *v1alpha1.Gateway) error
	GetGateway(ctx context.Context, atespace, name string) (*v1alpha1.Gateway, error)
	ListGateways(ctx context.Context, atespace string) ([]*v1alpha1.Gateway, error)
	DeleteGateway(ctx context.Context, atespace, name string) error

	SaveWorkspace(ctx context.Context, ws *v1alpha1.Workspace) error
	GetWorkspace(ctx context.Context, atespace, name string) (*v1alpha1.Workspace, error)
	ListWorkspaces(ctx context.Context, atespace string) ([]*v1alpha1.Workspace, error)
	DeleteWorkspace(ctx context.Context, atespace, name string) error

	SaveModel(ctx context.Context, model *v1alpha1.Model) error
	GetModel(ctx context.Context, atespace, name string) (*v1alpha1.Model, error)
	ListModels(ctx context.Context, atespace string) ([]*v1alpha1.Model, error)
	DeleteModel(ctx context.Context, atespace, name string) error

	WatchTask(ctx context.Context, atespace, name string) (<-chan *v1alpha1.Task, io.Closer, error)
	Close() error
}
