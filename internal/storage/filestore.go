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

package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// FileStore is a minimal filesystem-backed Store. Each key maps to one file
// under a root directory whose contents are the value.
//
// Scope and limitations:
//
//   - Durability across restarts: yes (files persist).
//   - Sharing across replicas: only if the root directory is itself shared and
//     consistent across them (e.g. a network filesystem). On purely local disk,
//     FileStore is single-node and is intended as the default for local and
//     single-replica use; use a managed backend for multi-replica deployments.
//   - Atomicity: writes go to a temp file and are atomically renamed into place,
//     so a reader never observes a torn value.
//
// FileStore assumes a single writer per key (see the package doc); it does not
// take OS file locks or provide compare-and-swap.
type FileStore struct {
	root string
}

var _ Store = (*FileStore)(nil)

// NewFileStore creates a FileStore rooted at dir, creating the directory (and
// parents) if needed.
func NewFileStore(dir string) (*FileStore, error) {
	if dir == "" {
		return nil, errors.New("storage: FileStore dir must not be empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("storage: creating root %q: %w", dir, err)
	}
	return &FileStore{root: dir}, nil
}

// path maps a key to its file path. The key is hashed so arbitrary key strings
// (slashes, etc.) map to a safe, fixed-length filename.
func (s *FileStore) path(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(s.root, hex.EncodeToString(sum[:])+".val")
}

// Get implements Store.Get.
func (s *FileStore) Get(ctx context.Context, key string) ([]byte, error) {
	if key == "" {
		return nil, errors.New("storage: empty key")
	}
	value, err := os.ReadFile(s.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("storage: reading key: %w", err)
	}
	return value, nil
}

// Put implements Store.Put (last-write-wins).
func (s *FileStore) Put(ctx context.Context, key string, value []byte) error {
	if key == "" {
		return errors.New("storage: empty key")
	}
	return s.atomicWrite(s.path(key), value)
}

// Delete implements Store.Delete (idempotent).
func (s *FileStore) Delete(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("storage: empty key")
	}
	if err := os.Remove(s.path(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("storage: deleting key: %w", err)
	}
	return nil
}

// atomicWrite writes value to a temp file and renames it into place so a reader
// never observes a partial write. It fsyncs the file before rename for
// durability.
func (s *FileStore) atomicWrite(path string, value []byte) error {
	tmp, err := os.CreateTemp(s.root, ".tmp-*")
	if err != nil {
		return fmt.Errorf("storage: creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op if the rename succeeded

	if _, err := tmp.Write(value); err != nil {
		tmp.Close()
		return fmt.Errorf("storage: writing temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("storage: syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("storage: closing temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("storage: renaming temp file into place: %w", err)
	}
	return nil
}
