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
	"time"

	"github.com/google/ax/internal/store"
)

// GetTaskVersion returns a deep copy of the stored row for key, or store.ErrNotFound.
func (s *MemoryStore) GetTaskVersion(ctx context.Context, key store.TaskKey) (*store.TaskVersion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key = key.Normalized()

	s.mu.RLock()
	defer s.mu.RUnlock()

	row, ok := s.tasks[taskKey(key.Atespace, key.Name)]
	if !ok || row == nil || row.Task == nil {
		return nil, store.ErrNotFound
	}
	cp := row.Clone()
	store.OverlayTaskGeneration(cp)
	return cp, nil
}

// Apply executes fn atomically under the store mutex and commits the returned mutation.
func (s *MemoryStore) Apply(ctx context.Context, key store.TaskKey, fn store.ApplyFunc) (*store.TaskVersion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key = key.Normalized()
	if key.Name == "" {
		return nil, errors.New("task name is required")
	}
	mapKey := taskKey(key.Atespace, key.Name)

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	stored := s.tasks[mapKey]

	// Pass a clone to fn so in-place mutations to current cannot corrupt
	// s.tasks[mapKey] or leak into the returned row if fn returns nil, current,
	// or an error.
	var passedCurrent *store.TaskVersion
	if stored != nil {
		passedCurrent = stored.Clone()
		store.OverlayTaskGeneration(passedCurrent)
	}

	next, err := fn(passedCurrent, now)
	switch {
	case err != nil:
		return nil, err

	case next == store.DeleteTaskVersion:
		delete(s.tasks, mapKey)
		return nil, nil

	case next == nil || (passedCurrent != nil && next == passedCurrent):
		if stored == nil {
			return nil, nil
		}
		ret := stored.Clone()
		store.OverlayTaskGeneration(ret)
		return ret, nil
	}

	written, err := store.PrepareTaskVersionForApply(next, stored, key)
	if err != nil {
		return nil, err
	}
	s.tasks[mapKey] = written
	return written.Clone(), nil
}
