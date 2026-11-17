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

package lock_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/ax/internal/lock"
)

func TestMemoryLocker_SerializesSameResource(t *testing.T) {
	locker := lock.NewMemoryLocker()
	ctx := context.Background()

	var counter int64
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock, err := locker.Lock(ctx, "task", "default", "my-task")
			if err != nil {
				t.Errorf("unexpected lock error: %v", err)
				return
			}
			defer unlock()

			current := atomic.AddInt64(&counter, 1)
			time.Sleep(10 * time.Millisecond)
			if atomic.LoadInt64(&counter) != current {
				t.Errorf("race condition detected: counter changed while lock held")
			}
		}()
	}

	wg.Wait()
}

func TestMemoryLocker_ConcurrentDifferentResources(t *testing.T) {
	locker := lock.NewMemoryLocker()
	ctx := context.Background()

	unlock1, err := locker.Lock(ctx, "task", "default", "task-1")
	if err != nil {
		t.Fatalf("lock task-1: %v", err)
	}
	defer unlock1()

	// Different resource should acquire immediately
	start := time.Now()
	unlock2, err := locker.Lock(ctx, "task", "default", "task-2")
	if err != nil {
		t.Fatalf("lock task-2: %v", err)
	}
	defer unlock2()

	if time.Since(start) > 50*time.Millisecond {
		t.Errorf("different resource lock was blocked")
	}
}

func TestMemoryLocker_Timeout(t *testing.T) {
	locker := lock.NewMemoryLocker()
	ctx := context.Background()

	unlock1, err := locker.Lock(ctx, "task", "default", "task-1")
	if err != nil {
		t.Fatalf("lock task-1: %v", err)
	}
	defer unlock1()

	timeoutCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()

	_, err = locker.Lock(timeoutCtx, "task", "default", "task-1")
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
}

func TestMemoryLocker_SharedRLockAndExclusiveLock(t *testing.T) {
	locker := lock.NewMemoryLocker()
	ctx := t.Context()

	// Multiple RLock holders acquire concurrently on the same resource.
	rUnlock1, err := locker.RLock(ctx, "workspace", "default", "ws-1")
	if err != nil {
		t.Fatalf("first RLock: %v", err)
	}
	rUnlock2, err := locker.RLock(ctx, "workspace", "default", "ws-1")
	if err != nil {
		t.Fatalf("second RLock: %v", err)
	}

	// Exclusive Lock must block while any RLock is held.
	shortCtx, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancel()
	if _, err := locker.Lock(shortCtx, "workspace", "default", "ws-1"); err == nil {
		t.Fatal("expected exclusive Lock to time out while RLock is held")
	}

	rUnlock1()
	shortCtx2, cancel2 := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancel2()
	if _, err := locker.Lock(shortCtx2, "workspace", "default", "ws-1"); err == nil {
		t.Fatal("expected exclusive Lock to remain blocked while second RLock is still held")
	}

	// Releasing the last RLock unblocks exclusive Lock, which in turn blocks RLock.
	rUnlock2()
	wUnlock, err := locker.Lock(ctx, "workspace", "default", "ws-1")
	if err != nil {
		t.Fatalf("Lock after all RLock released: %v", err)
	}
	shortCtx3, cancel3 := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancel3()
	if _, err := locker.RLock(shortCtx3, "workspace", "default", "ws-1"); err == nil {
		t.Fatal("expected RLock to time out while exclusive Lock is held")
	}
	wUnlock()

	rUnlock3, err := locker.RLock(ctx, "workspace", "default", "ws-1")
	if err != nil {
		t.Fatalf("RLock after exclusive Lock released: %v", err)
	}
	rUnlock3()
}
