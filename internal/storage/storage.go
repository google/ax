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

// Package storage defines a small durable key-value store used by components
// (such as harnesses) that must persist state that outlives the process.
//
// # Why this exists
//
// A harness that must survive process restarts cannot keep its durable state
// (for example, a conversation's resume cursor) in memory: the state would be
// lost on restart. This package is the seam through which such state is
// persisted. The interface is intentionally a minimal key-value store so it can
// be backed by anything from the local filesystem (the default, see FileStore)
// to a managed service (e.g. Firestore or GCS) without changing callers.
//
// # Concurrency model
//
// The Store does NOT provide compare-and-swap. Callers are expected to ensure a
// single writer per key (for the harness, the controller guarantees at most one
// Execution per conversation, so there is only ever one writer for a given
// conversation's key). Under that assumption a last-write-wins Put is correct.
//
// # Semantics every implementation MUST honor
//
//   - Atomicity: a Put is all-or-nothing. A reader never observes a torn/partial
//     value; it sees either the previous value or the new one.
//   - Read-after-write consistency: once Put returns success, a subsequent Get
//     observes that value (or a later one), never an older one.
//   - Not-found is distinct from failure: Get on a missing key returns
//     ErrNotFound, which MUST NOT be conflated with a backend error
//     (unavailable, permission denied, ...). Callers rely on ErrNotFound to mean
//     "no state yet" and on other errors to mean "could not determine state"
//     (which must not be treated as "no state").
//   - Durability: when Put returns success, the value is durably persisted
//     (survives process restart).
//
// Keys are opaque non-empty strings; values are opaque byte slices (callers
// choose the encoding).
package storage

import (
	"context"
	"errors"
)

// ErrNotFound is returned by Get when the key has no stored value. It is
// distinct from a backend failure: it means "no state yet", not "lookup failed".
var ErrNotFound = errors.New("storage: key not found")

// Store is a durable key-value store. See the package doc for the semantics
// every implementation must satisfy. Implementations must be safe for concurrent
// use by multiple goroutines.
type Store interface {
	// Get returns the value stored under key. It returns ErrNotFound if the key
	// has no value; any other error indicates the lookup could not be completed
	// (and MUST NOT be interpreted as "absent").
	Get(ctx context.Context, key string) ([]byte, error)

	// Put stores value under key (last-write-wins).
	Put(ctx context.Context, key string, value []byte) error

	// Delete removes key. Deleting a missing key is not an error (it is
	// idempotent).
	Delete(ctx context.Context, key string) error
}
