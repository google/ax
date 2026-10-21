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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// fileLockPollInterval is how often FileLocker retries a contended lock.
const fileLockPollInterval = 25 * time.Millisecond

// FileLocker is a Locker backed by flock(2) on files in a directory. It
// serializes operations on the same resource across processes on one host,
// which is what the ax CLI needs in local mode, where every invocation runs
// its own in-process server against a shared database.
//
// flock locks belong to the open file description, so two Lock calls in the
// same process exclude each other as well.
type FileLocker struct {
	dir string
}

// NewFileLocker returns a FileLocker that keeps its lock files in dir,
// creating it with owner-only permissions if needed.
func NewFileLocker(dir string) (*FileLocker, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating lock directory: %w", err)
	}
	return &FileLocker{dir: dir}, nil
}

// Lock blocks until the lock for the resource is held or ctx is done.
func (l *FileLocker) Lock(ctx context.Context, kind, atespace, name string) (func(), error) {
	sum := sha256.Sum256([]byte(Key(kind, atespace, name)))
	path := filepath.Join(l.dir, hex.EncodeToString(sum[:16])+".lock")

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%w: opening %s: %v", ErrLockFailed, path, err)
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EINTR {
			_ = f.Close()
			return nil, fmt.Errorf("%w: %v", ErrLockFailed, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("%w: %v", ErrLockFailed, ctx.Err())
		case <-time.After(fileLockPollInterval):
		}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
