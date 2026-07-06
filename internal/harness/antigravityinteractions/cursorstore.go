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

package antigravityinteractions

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// resumeCursor is the small per-conversation state persisted so a conversation
// can resume across restarts/replicas. It records the tail of the server-side
// interaction chain (PrevInteractionID).
//
// It is a struct (rather than a bare string) so it can grow to hold richer
// resume state later, e.g. partial function-call results for mid-tool-loop
// recovery, without changing the on-disk shape's identity.
type resumeCursor struct {
	PrevInteractionID string `json:"prev_interaction_id"`
}

// cursorStore is a minimal filesystem-backed store for resume cursors, local to
// the Antigravity Interactions harness. Each conversation maps to one file whose
// contents are the JSON-encoded cursor.
//
// It assumes a single writer per conversation (the controller guarantees at most
// one Execution per conversation), so writes are last-write-wins with no
// compare-and-swap. Writes are atomic (temp file + rename) so a reader never
// observes a torn value.
type cursorStore struct {
	dir string
}

// newCursorStore creates a cursorStore rooted at dir, creating it if needed.
func newCursorStore(dir string) (*cursorStore, error) {
	if dir == "" {
		return nil, errors.New("cursorStore dir must not be empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating cursor dir %q: %w", dir, err)
	}
	return &cursorStore{dir: dir}, nil
}

// path maps a conversation id to its cursor file. The id is hashed so arbitrary
// id strings map to a safe, fixed-length filename.
func (s *cursorStore) path(conversationID string) string {
	sum := sha256.Sum256([]byte(conversationID))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:])+".json")
}

// load returns the stored cursor for conversationID. found is false if no cursor
// has been persisted yet; a non-nil error means the lookup itself failed (which
// callers must not treat as "no cursor").
func (s *cursorStore) load(conversationID string) (cur resumeCursor, found bool, err error) {
	blob, err := os.ReadFile(s.path(conversationID))
	if errors.Is(err, os.ErrNotExist) {
		return resumeCursor{}, false, nil
	}
	if err != nil {
		return resumeCursor{}, false, fmt.Errorf("reading cursor: %w", err)
	}
	if err := json.Unmarshal(blob, &cur); err != nil {
		return resumeCursor{}, false, fmt.Errorf("decoding cursor: %w", err)
	}
	return cur, true, nil
}

// save durably writes the cursor for conversationID (last-write-wins).
func (s *cursorStore) save(conversationID string, cur resumeCursor) error {
	blob, err := json.Marshal(cur)
	if err != nil {
		return fmt.Errorf("encoding cursor: %w", err)
	}
	return s.atomicWrite(s.path(conversationID), blob)
}

// atomicWrite writes value to a temp file, fsyncs it, and renames it into place
// so a reader never observes a partial write.
func (s *cursorStore) atomicWrite(path string, value []byte) error {
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op if the rename succeeded

	if _, err := tmp.Write(value); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming temp file into place: %w", err)
	}
	return nil
}
