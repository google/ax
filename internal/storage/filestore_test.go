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
	"errors"
	"testing"
)

func newTestStore(t *testing.T) *FileStore {
	t.Helper()
	s, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	return s
}

func TestGetNotFound(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Get(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
}

func TestPutThenGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Put(ctx, "k", []byte("hello")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("Get value = %q, want %q", got, "hello")
	}
}

func TestPutOverwrites(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Put(ctx, "k", []byte("v1")); err != nil {
		t.Fatalf("Put v1: %v", err)
	}
	if err := s.Put(ctx, "k", []byte("v2")); err != nil {
		t.Fatalf("Put v2: %v", err)
	}
	got, _ := s.Get(ctx, "k")
	if string(got) != "v2" {
		t.Fatalf("Get value = %q, want %q", got, "v2")
	}
}

func TestDeleteIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Delete(ctx, "missing"); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
	if err := s.Put(ctx, "k", []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete existing: %v", err)
	}
	if _, err := s.Get(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
}

// TestPersistsAcrossReopen verifies a value survives reconstructing the store
// over the same directory (the restart scenario).
func TestPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	s1, _ := NewFileStore(dir)
	if err := s1.Put(ctx, "k", []byte("durable")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	s2, _ := NewFileStore(dir) // simulate a process restart
	got, err := s2.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if string(got) != "durable" {
		t.Fatalf("value after reopen = %q, want %q", got, "durable")
	}
}
