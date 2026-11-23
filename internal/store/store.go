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
	"google.golang.org/protobuf/proto"
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

// StoredStatus returns the part of a task's status that AX owns and stores. The
// actor name, actor UID, and (for states outside [v1alpha1.IsAXTaskState]) the
// state, status detail, and Ready condition that follow from the actor's live
// state on Agent Substrate are dropped. st is not modified; a nil st returns an
// empty non-nil TaskStatus.
func StoredStatus(st *v1alpha1.TaskStatus) *v1alpha1.TaskStatus {
	out := &v1alpha1.TaskStatus{}
	if st != nil {
		out = proto.Clone(st).(*v1alpha1.TaskStatus)
	}
	out.Actor = ""
	out.ActorUid = ""
	if !v1alpha1.IsAXTaskState(out.State) {
		out.State = v1alpha1.TaskState_TASK_STATE_UNSPECIFIED
		out.StatusDetail = nil
		conds := out.Conditions[:0]
		for _, c := range out.Conditions {
			if c.GetType() != v1alpha1.ConditionReady {
				conds = append(conds, c)
			}
		}
		out.Conditions = conds
	}
	return out
}
