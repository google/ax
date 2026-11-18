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

package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadCapturedTaskOutput_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("top-secret-token"), 0o600); err != nil {
		t.Fatalf("WriteFile(secret): %v", err)
	}

	axDir := filepath.Join(dir, ".ax")
	if err := os.MkdirAll(axDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(.ax): %v", err)
	}
	outLink := filepath.Join(axDir, "output")
	if err := os.Symlink(secret, outLink); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	tail := newTailBuffer(MaxOutputBytes)
	_, _ = tail.Write([]byte("fallback-tail\n"))

	if got := readCapturedTaskOutput(outLink, tail); got != "fallback-tail" {
		t.Fatalf("readCapturedTaskOutput(symlink) = %q, want %q", got, "fallback-tail")
	}

	// Regular file owned by the expected user is accepted.
	if err := os.Remove(outLink); err != nil {
		t.Fatalf("Remove(outLink): %v", err)
	}
	if err := os.WriteFile(outLink, []byte("agent-output\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(output): %v", err)
	}
	if got := readCapturedTaskOutput(outLink, tail); got != "agent-output" {
		t.Fatalf("readCapturedTaskOutput(regular) = %q, want %q", got, "agent-output")
	}
}
