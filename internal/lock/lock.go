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

package lock

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// TODO(jbd): Move Locker interface and implementations
// to storage.

var (
	ErrLockFailed = errors.New("failed to acquire lock")
)

// Locker provides exclusive locking per resource kind, atespace, and name.
//
// When ax-server runs with more than one replica, every replica must use a
// Locker backed by the same shared system. Distributed implementations are
// provided by storage packages (for example postgres.Store) so that locks live
// next to the data they protect.
type Locker interface {
	// Lock acquires an exclusive lock for the specified resource.
	// It blocks until the lock is acquired or ctx is cancelled.
	// The returned function releases the lock.
	Lock(ctx context.Context, kind, atespace, name string) (unlock func(), err error)
}

// Key formats the standard lock key.
func Key(kind, atespace, name string) string {
	if atespace == "" {
		atespace = "default"
	}
	return fmt.Sprintf("lock:%s:%s:%s", kind, atespace, name)
}

// MemoryLocker is an in-memory keyed mutex implementation suitable for tests and local mode.
type MemoryLocker struct {
	mu    sync.Mutex
	locks map[string]*entry
}

type entry struct {
	mu  sync.Mutex
	ref int
}

// NewMemoryLocker creates an in-memory Locker.
func NewMemoryLocker() *MemoryLocker {
	return &MemoryLocker{
		locks: make(map[string]*entry),
	}
}

func (m *MemoryLocker) Lock(ctx context.Context, kind, atespace, name string) (func(), error) {
	key := Key(kind, atespace, name)

	m.mu.Lock()
	e, ok := m.locks[key]
	if !ok {
		e = &entry{}
		m.locks[key] = e
	}
	e.ref++
	m.mu.Unlock()

	// Acquire lock with ctx awareness
	locked := make(chan struct{})
	go func() {
		e.mu.Lock()
		close(locked)
	}()

	select {
	case <-ctx.Done():
		// Clean up ref if timed out waiting
		go func() {
			<-locked
			e.mu.Unlock()
			m.mu.Lock()
			e.ref--
			if e.ref == 0 {
				delete(m.locks, key)
			}
			m.mu.Unlock()
		}()
		return nil, ctx.Err()
	case <-locked:
	}

	var once sync.Once
	unlock := func() {
		once.Do(func() {
			e.mu.Unlock()
			m.mu.Lock()
			e.ref--
			if e.ref == 0 {
				delete(m.locks, key)
			}
			m.mu.Unlock()
		})
	}
	return unlock, nil
}
