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
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/google/ax/internal/harness"
)

// Registry manages a collection of harnesses.
type Registry struct {
	mu        sync.RWMutex
	harnesses map[string]harness.Harness
}

// NewRegistry creates a new harness registry.
func NewRegistry() *Registry {
	return &Registry{
		harnesses: make(map[string]harness.Harness),
	}
}

// RegisterHarness registers a harness under the given id. An empty id registers
// the harness as the default, used when a request specifies no agent id.
func (r *Registry) RegisterHarness(id string, h harness.Harness) error {
	if id != "" {
		if err := validateID(id); err != nil {
			return err
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.harnesses[id]; ok {
		return fmt.Errorf("harness %q already registered", id)
	}
	r.harnesses[id] = h
	return nil
}

// Harness retrieves a harness by id.
func (r *Registry) Harness(id string) (harness.Harness, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.harnesses[id]
	if !ok {
		return nil, fmt.Errorf("harness %s not found", id)
	}
	return h, nil
}

// Close releases resources held by the registry. Any harness that implements
// io.Closer is closed exactly once, even if it was registered under multiple
// IDs (e.g. under its real ID and again under "" as the default). Errors from
// individual harness Close calls are joined so a single failure does not
// suppress the rest.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	seen := make(map[harness.Harness]struct{}, len(r.harnesses))
	var errs []error
	for _, h := range r.harnesses {
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		closer, ok := h.(io.Closer)
		if !ok {
			continue
		}
		if err := closer.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
