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

// Locker provides shared and exclusive locking per resource kind, atespace, and
// name.
//
// When ax-server runs with more than one replica, every replica must use a
// Locker backed by the same shared system. Distributed implementations are
// provided by storage packages (for example postgres.Store) so that locks live
// next to the data they protect.
//
// Semantics across implementations:
//   - Non-reentrant: neither Lock nor RLock is re-entrant, and a held RLock
//     cannot be upgraded to Lock on the same key (doing so may deadlock).
//   - Fairness is implementation-defined: MemoryLocker (sync.RWMutex) favors
//     waiting writers, whereas postgres.Store favors readers because waiting
//     exclusive callers poll without reserving a row (so an exclusive operation
//     such as DeleteWorkspace may wait behind overlapping cold-miss builds).
type Locker interface {
	// Lock acquires an exclusive lock for the specified resource.
	// It blocks until no other shared or exclusive locks are held on the
	// resource, or ctx is cancelled. The returned function releases the lock.
	Lock(ctx context.Context, kind, atespace, name string) (unlock func(), err error)

	// RLock acquires a shared read lock for the specified resource.
	// Multiple callers may hold RLock concurrently on the same resource, while
	// any exclusive Lock call waits until all RLock holders release (and vice
	// versa). The returned function releases the shared lock.
	RLock(ctx context.Context, kind, atespace, name string) (unlock func(), err error)
}

// Key formats the standard lock key.
func Key(kind, atespace, name string) string {
	if atespace == "" {
		atespace = "default"
	}
	return fmt.Sprintf("lock:%s:%s:%s", kind, atespace, name)
}

// MemoryLocker is an in-memory keyed read/write mutex implementation suitable
// for tests and single-process local mode.
type MemoryLocker struct {
	mu    sync.Mutex
	locks map[string]*entry
}

type entry struct {
	mu  sync.RWMutex
	ref int
}

// NewMemoryLocker creates an in-memory Locker.
func NewMemoryLocker() *MemoryLocker {
	return &MemoryLocker{
		locks: make(map[string]*entry),
	}
}

// Lock acquires an exclusive lock on (kind, atespace, name).
func (m *MemoryLocker) Lock(ctx context.Context, kind, atespace, name string) (func(), error) {
	return m.acquire(ctx, kind, atespace, name, false)
}

// RLock acquires a shared lock on (kind, atespace, name).
func (m *MemoryLocker) RLock(ctx context.Context, kind, atespace, name string) (func(), error) {
	return m.acquire(ctx, kind, atespace, name, true)
}

func (m *MemoryLocker) acquire(ctx context.Context, kind, atespace, name string, shared bool) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := Key(kind, atespace, name)

	m.mu.Lock()
	e, ok := m.locks[key]
	if !ok {
		e = &entry{}
		m.locks[key] = e
	}
	e.ref++
	m.mu.Unlock()

	lockFn := e.mu.Lock
	unlockFn := e.mu.Unlock
	if shared {
		lockFn = e.mu.RLock
		unlockFn = e.mu.RUnlock
	}

	locked := make(chan struct{})
	go func() {
		lockFn()
		close(locked)
	}()

	releaseRef := func() {
		unlockFn()
		m.mu.Lock()
		e.ref--
		if e.ref == 0 {
			delete(m.locks, key)
		}
		m.mu.Unlock()
	}

	select {
	case <-ctx.Done():
		go func() {
			<-locked
			releaseRef()
		}()
		return nil, ctx.Err()
	case <-locked:
	}

	var once sync.Once
	return func() {
		once.Do(releaseRef)
	}, nil
}
