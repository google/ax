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

package controller

import (
	"context"
	"testing"

	"github.com/google/ax/internal/harness"
)

type dummyHarness struct{}

func (d *dummyHarness) Start(ctx context.Context, conversationID string, harnessConfig []byte) (harness.Execution, error) {
	return nil, nil
}

func TestRegistry_RegisterHarness(t *testing.T) {
	r := NewRegistry()
	h := &dummyHarness{}

	if err := r.RegisterHarness("antigravity", h); err != nil {
		t.Fatalf("RegisterHarness(valid id): %v", err)
	}

	// Duplicate id is rejected.
	if err := r.RegisterHarness("antigravity", h); err == nil {
		t.Error("expected error registering duplicate id, got nil")
	}

	// Invalid id is rejected.
	if err := r.RegisterHarness("bad id", h); err == nil {
		t.Error("expected error registering invalid id, got nil")
	}

	// Empty id is reserved for the default harness.
	if err := r.RegisterHarness("", h); err == nil {
		t.Error("expected error registering empty id, got nil")
	}
}

func TestRegistry_FindHarness(t *testing.T) {
	r := NewRegistry()
	h := &dummyHarness{}
	if err := r.RegisterHarness("antigravity", h); err != nil {
		t.Fatalf("RegisterHarness: %v", err)
	}

	if _, err := r.Harness("antigravity"); err != nil {
		t.Errorf("Harness(antigravity): %v", err)
	}
	if _, err := r.Harness("missing"); err == nil {
		t.Error("expected error looking up missing harness, got nil")
	}
}

func TestRegistry_SetDefaultHarness(t *testing.T) {
	r := NewRegistry()
	if err := r.RegisterHarness("antigravity", &dummyHarness{}); err != nil {
		t.Fatalf("RegisterHarness: %v", err)
	}

	// An unregistered id is rejected.
	if err := r.SetDefaultHarness("missing"); err == nil {
		t.Error("SetDefaultHarness(missing): expected error, got nil")
	}

	// A registered id becomes the default.
	if err := r.SetDefaultHarness("antigravity"); err != nil {
		t.Fatalf("SetDefaultHarness(antigravity): %v", err)
	}
	if r.defaultHarness != "antigravity" {
		t.Errorf("defaultHarness = %q, want %q", r.defaultHarness, "antigravity")
	}
}
