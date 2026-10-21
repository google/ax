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
	"testing"
	"time"
)

func TestFileLockerExcludesAndReleases(t *testing.T) {
	l, err := NewFileLocker(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	unlock, err := l.Lock(ctx, "task", "default", "a")
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	// A different resource is independent.
	unlockB, err := l.Lock(ctx, "task", "default", "b")
	if err != nil {
		t.Fatalf("Lock(b): %v", err)
	}
	unlockB()

	// The same resource blocks until released or the context ends.
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := l.Lock(short, "task", "default", "a"); !errors.Is(err, ErrLockFailed) {
		t.Fatalf("contended Lock err = %v, want ErrLockFailed", err)
	}

	acquired := make(chan struct{})
	go func() {
		u, err := l.Lock(ctx, "task", "default", "a")
		if err == nil {
			u()
		}
		close(acquired)
	}()
	time.Sleep(50 * time.Millisecond)
	unlock()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not acquire the lock after release")
	}
}
