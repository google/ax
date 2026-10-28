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

package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/google/ax/internal/store"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/proto"
)

// clone returns a deep copy of a protobuf message so stored resources are never
// shared with callers. Copying the struct by value is unsafe: generated messages
// embed internal state, including a mutex.
func clone[T proto.Message](m T) T {
	return proto.Clone(m).(T)
}

// MemoryStore is an in-memory implementation of store.Store for testing and single-node setups.
type MemoryStore struct {
	mu         sync.RWMutex
	tasks      map[string]*v1alpha1.Task
	models     map[string]*v1alpha1.Model
	workspaces map[string]*v1alpha1.Workspace
}

// NewStore creates a new in-memory Store.
func NewStore() *MemoryStore {
	return &MemoryStore{
		tasks:      make(map[string]*v1alpha1.Task),
		models:     make(map[string]*v1alpha1.Model),
		workspaces: make(map[string]*v1alpha1.Workspace),
	}
}

// sortByKey orders resources by atespace and name, so that lists, and pages
// of them, come back in a stable order.
func sortByKey[T interface{ GetMetadata() *v1alpha1.ObjectMeta }](items []T) {
	sort.Slice(items, func(i, j int) bool {
		return metaKey(items[i].GetMetadata()) < metaKey(items[j].GetMetadata())
	})
}

func metaKey(m *v1alpha1.ObjectMeta) string {
	return taskKey(m.GetAtespace(), m.GetName())
}

func taskKey(atespace, name string) string {
	if atespace == "" {
		atespace = "default"
	}
	return fmt.Sprintf("%s:%s", atespace, name)
}

func (s *MemoryStore) SaveTask(ctx context.Context, task *v1alpha1.Task) error {
	if task.Metadata == nil {
		task.Metadata = &v1alpha1.ObjectMeta{}
	}
	if task.Metadata.Name == "" {
		return errors.New("task name is required")
	}
	if task.Metadata.Atespace == "" {
		task.Metadata.Atespace = "default"
	}
	if task.ApiVersion == "" {
		task.ApiVersion = v1alpha1.APIVersion
	}
	if task.Kind == "" {
		task.Kind = v1alpha1.KindTask
	}
	if task.Status == nil {
		task.Status = &v1alpha1.TaskStatus{}
	}
	if task.Status.Phase == "" {
		task.Status.Phase = v1alpha1.PhasePending
	}

	key := taskKey(task.Metadata.Atespace, task.Metadata.Name)

	s.mu.Lock()
	s.tasks[key] = clone(task)
	s.mu.Unlock()

	return nil
}

func (s *MemoryStore) GetTask(ctx context.Context, atespace, name string) (*v1alpha1.Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	key := taskKey(atespace, name)
	t, ok := s.tasks[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := clone(t)
	return cp, nil
}

func (s *MemoryStore) ListTasks(ctx context.Context, atespace string, limit, offset int64) ([]*v1alpha1.Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*v1alpha1.Task
	for _, t := range s.tasks {
		if atespace == "" || atespace == "*" || t.Metadata.Atespace == atespace {
			cp := clone(t)
			result = append(result, cp)
		}
	}
	sortByKey(result)

	if offset >= int64(len(result)) {
		return []*v1alpha1.Task{}, nil
	}
	end := offset + limit
	if limit <= 0 {
		end = offset + 50
	}
	if end > int64(len(result)) {
		end = int64(len(result))
	}
	return result[offset:end], nil
}

func (s *MemoryStore) UpdateTaskStatus(ctx context.Context, atespace, name string, status *v1alpha1.TaskStatus) error {
	key := taskKey(atespace, name)

	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[key]
	if !ok {
		return store.ErrNotFound
	}
	t.Status = status
	return nil
}

func (s *MemoryStore) DeleteTask(ctx context.Context, atespace, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, taskKey(atespace, name))
	return nil
}

func (s *MemoryStore) SaveModel(ctx context.Context, model *v1alpha1.Model) error {
	if model.Metadata == nil {
		model.Metadata = &v1alpha1.ObjectMeta{}
	}
	if model.Metadata.Name == "" {
		return errors.New("model name is required")
	}
	if model.Metadata.Atespace == "" {
		model.Metadata.Atespace = "default"
	}
	if model.ApiVersion == "" {
		model.ApiVersion = v1alpha1.APIVersion
	}
	if model.Kind == "" {
		model.Kind = v1alpha1.KindModel
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := clone(model)
	s.models[metaKey(model.Metadata)] = cp
	return nil
}

func (s *MemoryStore) GetModel(ctx context.Context, atespace, name string) (*v1alpha1.Model, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	m, ok := s.models[taskKey(atespace, name)]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := clone(m)
	return cp, nil
}

func (s *MemoryStore) ListModels(ctx context.Context, atespace string) ([]*v1alpha1.Model, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*v1alpha1.Model
	for _, m := range s.models {
		if atespace == "" || atespace == "*" || m.Metadata.Atespace == atespace {
			cp := clone(m)
			result = append(result, cp)
		}
	}
	sortByKey(result)
	return result, nil
}

func (s *MemoryStore) DeleteModel(ctx context.Context, atespace, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.models, taskKey(atespace, name))
	return nil
}

func (s *MemoryStore) SaveWorkspace(ctx context.Context, ws *v1alpha1.Workspace) error {
	if ws.Metadata == nil {
		ws.Metadata = &v1alpha1.ObjectMeta{}
	}
	if ws.Metadata.Name == "" {
		return errors.New("workspace name is required")
	}
	if ws.Metadata.Atespace == "" {
		ws.Metadata.Atespace = "default"
	}
	if ws.ApiVersion == "" {
		ws.ApiVersion = v1alpha1.APIVersion
	}
	if ws.Kind == "" {
		ws.Kind = v1alpha1.KindWorkspace
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := clone(ws)
	s.workspaces[metaKey(ws.Metadata)] = cp
	return nil
}

func (s *MemoryStore) GetWorkspace(ctx context.Context, atespace, name string) (*v1alpha1.Workspace, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ws, ok := s.workspaces[taskKey(atespace, name)]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := clone(ws)
	return cp, nil
}

func (s *MemoryStore) ListWorkspaces(ctx context.Context, atespace string) ([]*v1alpha1.Workspace, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*v1alpha1.Workspace
	for _, w := range s.workspaces {
		if atespace == "" || atespace == "*" || w.Metadata.Atespace == atespace {
			cp := clone(w)
			result = append(result, cp)
		}
	}
	sortByKey(result)
	return result, nil
}

func (s *MemoryStore) DeleteWorkspace(ctx context.Context, atespace, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.workspaces, taskKey(atespace, name))
	return nil
}

func (s *MemoryStore) Close() error {
	return nil
}
