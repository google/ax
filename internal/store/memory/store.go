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
	"io"
	"sync"
	"time"

	"github.com/google/ax/internal/store"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/proto"
)

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

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
	gateways   map[string]*v1alpha1.Gateway
	models     map[string]*v1alpha1.Model
	workspaces map[string]*v1alpha1.Workspace
	events     chan store.TaskEvent
	watchers   map[string][]chan *v1alpha1.Task
}

// NewStore creates a new in-memory Store.
func NewStore() *MemoryStore {
	return &MemoryStore{
		tasks:      make(map[string]*v1alpha1.Task),
		gateways:   make(map[string]*v1alpha1.Gateway),
		models:     make(map[string]*v1alpha1.Model),
		workspaces: make(map[string]*v1alpha1.Workspace),
		events:     make(chan store.TaskEvent, 1000),
		watchers:   make(map[string][]chan *v1alpha1.Task),
	}
}

func taskKey(atespace, name string) string {
	if atespace == "" {
		atespace = "default"
	}
	return fmt.Sprintf("%s:%s", atespace, name)
}

func (s *MemoryStore) CreateTask(ctx context.Context, task *v1alpha1.Task) error {
	if task.Metadata == nil {
		task.Metadata = &v1alpha1.ObjectMeta{}
	}
	if task.Metadata.Name == "" {
		return errors.New("task name is required")
	}
	if task.Metadata.Atespace == "" {
		task.Metadata.Atespace = "default"
	}
	if task.Status == nil {
		task.Status = &v1alpha1.TaskStatus{}
	}
	if task.Status.Phase == "" {
		task.Status.Phase = "Pending"
	}

	key := taskKey(task.Metadata.Atespace, task.Metadata.Name)

	s.mu.Lock()
	if _, exists := s.tasks[key]; exists {
		s.mu.Unlock()
		return store.ErrAlreadyExists
	}
	cp := clone(task)
	s.tasks[key] = cp

	event := store.TaskEvent{
		ID:       fmt.Sprintf("%d", time.Now().UnixNano()),
		Atespace: task.Metadata.Atespace,
		Name:     task.Metadata.Name,
		Action:   "reconcile",
	}

	// Notify watchers
	if chs, ok := s.watchers[key]; ok {
		for _, ch := range chs {
			select {
			case ch <- cp:
			default:
			}
		}
	}
	s.mu.Unlock()

	select {
	case s.events <- event:
	default:
	}

	return nil
}

func (s *MemoryStore) PublishEvent(ctx context.Context, ev store.TaskEvent) error {
	if ev.ID == "" {
		ev.ID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	if ev.Atespace == "" {
		ev.Atespace = "default"
	}
	select {
	case s.events <- ev:
	default:
	}
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

	if offset >= int64(len(result)) {
		return []*v1alpha1.Task{}, nil
	}
	end := offset + limit
	if limit <= 0 || end > int64(len(result)) {
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
	cp := clone(t)

	if chs, ok := s.watchers[key]; ok {
		for _, ch := range chs {
			select {
			case ch <- cp:
			default:
			}
		}
	}
	return nil
}

func (s *MemoryStore) DeleteTask(ctx context.Context, atespace, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, taskKey(atespace, name))
	return nil
}

func (s *MemoryStore) SaveGateway(ctx context.Context, gw *v1alpha1.Gateway) error {
	if gw.Metadata.Name == "" {
		return errors.New("gateway name is required")
	}
	if gw.Metadata.Atespace == "" {
		gw.Metadata.Atespace = "default"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := clone(gw)
	s.gateways[taskKey(gw.Metadata.Atespace, gw.Metadata.Name)] = cp
	return nil
}

func (s *MemoryStore) GetGateway(ctx context.Context, atespace, name string) (*v1alpha1.Gateway, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	gw, ok := s.gateways[taskKey(atespace, name)]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := clone(gw)
	return cp, nil
}

func (s *MemoryStore) ListGateways(ctx context.Context, atespace string) ([]*v1alpha1.Gateway, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*v1alpha1.Gateway
	for _, g := range s.gateways {
		if atespace == "" || atespace == "*" || g.Metadata.Atespace == atespace {
			cp := clone(g)
			result = append(result, cp)
		}
	}
	return result, nil
}

func (s *MemoryStore) DeleteGateway(ctx context.Context, atespace, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.gateways, taskKey(atespace, name))
	return nil
}

func (s *MemoryStore) SaveModel(ctx context.Context, model *v1alpha1.Model) error {
	if model.Metadata.Name == "" {
		return errors.New("model name is required")
	}
	if model.Metadata.Atespace == "" {
		model.Metadata.Atespace = "default"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := clone(model)
	s.models[taskKey(model.Metadata.Atespace, model.Metadata.Name)] = cp
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
	return result, nil
}

func (s *MemoryStore) DeleteModel(ctx context.Context, atespace, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.models, taskKey(atespace, name))
	return nil
}

func (s *MemoryStore) SaveWorkspace(ctx context.Context, ws *v1alpha1.Workspace) error {
	if ws.Metadata.Name == "" {
		return errors.New("workspace name is required")
	}
	if ws.Metadata.Atespace == "" {
		ws.Metadata.Atespace = "default"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := clone(ws)
	s.workspaces[taskKey(ws.Metadata.Atespace, ws.Metadata.Name)] = cp
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
	return result, nil
}

func (s *MemoryStore) DeleteWorkspace(ctx context.Context, atespace, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.workspaces, taskKey(atespace, name))
	return nil
}

// Subscribe returns a subscription over the store's single event channel. Every
// subscription shares that channel, so each event reaches exactly one subscriber,
// which matches the group semantics of the Redis implementation. Groups and
// consumers are accepted for interface parity but carry no meaning here.
func (s *MemoryStore) Subscribe(ctx context.Context, group, consumer string) (store.Subscription, error) {
	return &subscription{events: s.events}, nil
}

type subscription struct {
	events <-chan store.TaskEvent
}

func (sub *subscription) Next(ctx context.Context) (store.TaskEvent, error) {
	select {
	case ev := <-sub.events:
		return ev, nil
	case <-ctx.Done():
		return store.TaskEvent{}, ctx.Err()
	}
}

// Ack is a no-op: the channel hands each event out once, so there is nothing to retain.
func (sub *subscription) Ack(ctx context.Context, ev store.TaskEvent) error {
	return nil
}

func (sub *subscription) Close() error {
	return nil
}

func (s *MemoryStore) WatchTask(ctx context.Context, atespace, name string) (<-chan *v1alpha1.Task, io.Closer, error) {
	key := taskKey(atespace, name)
	ch := make(chan *v1alpha1.Task, 10)

	s.mu.Lock()
	s.watchers[key] = append(s.watchers[key], ch)
	s.mu.Unlock()

	closer := closerFunc(func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		chs := s.watchers[key]
		for i, c := range chs {
			if c == ch {
				s.watchers[key] = append(chs[:i], chs[i+1:]...)
				close(ch)
				break
			}
		}
		return nil
	})

	return ch, closer, nil
}

func (s *MemoryStore) Close() error {
	return nil
}
