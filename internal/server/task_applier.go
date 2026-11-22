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

package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/ax/internal/store"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/proto"
)

// TaskApplierForStore returns s as a [store.TaskApplier] when supported, or
// wraps s in a fallback adapter for test doubles that only implement [store.Store].
func TaskApplierForStore(s store.Store) store.TaskApplier {
	if applier, ok := s.(store.TaskApplier); ok {
		return applier
	}
	return &storeTaskApplier{
		store: s,
		rows:  make(map[store.TaskKey]*store.TaskVersion),
	}
}

type storeTaskApplier struct {
	store store.Store
	mu    sync.Mutex
	rows  map[store.TaskKey]*store.TaskVersion
}

func (a *storeTaskApplier) GetTaskVersion(ctx context.Context, key store.TaskKey) (*store.TaskVersion, error) {
	key = key.Normalized()
	task, err := a.store.GetTask(ctx, key.Atespace, key.Name)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if tracked, ok := a.rows[key]; ok {
		out := tracked.Clone()
		out.Task = proto.Clone(task).(*v1alpha1.Task)
		return out, nil
	}
	return &store.TaskVersion{
		Task:       proto.Clone(task).(*v1alpha1.Task),
		Generation: task.GetStatus().GetGeneration(),
	}, nil
}

func (a *storeTaskApplier) Apply(ctx context.Context, key store.TaskKey, fn store.ApplyFunc) (*store.TaskVersion, error) {
	key = key.Normalized()
	a.mu.Lock()
	defer a.mu.Unlock()

	current, err := a.loadRowLocked(ctx, key)
	if err != nil {
		return nil, err
	}

	var before *store.TaskVersion
	if current != nil {
		before = current.Clone()
	}

	next, err := fn(current, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if next == nil || next == current {
		return before, nil
	}
	if next == store.DeleteTaskVersion {
		if current == nil {
			return nil, nil
		}
		if err := a.store.DeleteTask(ctx, key.Atespace, key.Name); err != nil {
			return nil, err
		}
		delete(a.rows, key)
		return nil, nil
	}
	if next.Task == nil {
		return nil, fmt.Errorf("apply returned TaskVersion with nil Task for %s/%s", key.Atespace, key.Name)
	}

	written := next.Clone()
	if written.Task.Metadata == nil {
		written.Task.Metadata = &v1alpha1.ObjectMeta{}
	}
	written.Task.Metadata.Atespace = key.Atespace
	written.Task.Metadata.Name = key.Name

	if current == nil {
		written.Generation = 1
	} else {
		written.Generation = current.Generation + 1
	}
	written.Task.Status = store.StoredStatus(written.Task.GetStatus())
	written.Task.Status.Generation = written.Generation

	if current == nil {
		if err := a.store.SaveTask(ctx, written.Task); err != nil {
			return nil, err
		}
	} else {
		if !proto.Equal(current.Task.GetMetadata().GetDeleteTime(), written.Task.GetMetadata().GetDeleteTime()) {
			if err := a.store.SaveTask(ctx, proto.Clone(written.Task).(*v1alpha1.Task)); err != nil {
				return nil, err
			}
		}
		if err := a.store.UpdateTaskStatus(ctx, key.Atespace, key.Name, written.Task.Status); err != nil {
			return nil, err
		}
	}
	a.rows[key] = written.Clone()
	return written, nil
}

func (a *storeTaskApplier) loadRowLocked(ctx context.Context, key store.TaskKey) (*store.TaskVersion, error) {
	task, err := a.store.GetTask(ctx, key.Atespace, key.Name)
	if errors.Is(err, store.ErrNotFound) {
		delete(a.rows, key)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if tracked, ok := a.rows[key]; ok {
		out := tracked.Clone()
		out.Task = proto.Clone(task).(*v1alpha1.Task)
		return out, nil
	}
	return &store.TaskVersion{
		Task:       proto.Clone(task).(*v1alpha1.Task),
		Generation: task.GetStatus().GetGeneration(),
	}, nil
}
