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
	"strings"
	"sync"
)

// MaxOutputBytes caps both the captured stdout/stderr tail and the
// $AX_OUTPUT_FILE payload reported in CommandExited.output (64 KiB).
const MaxOutputBytes = 64 << 10

// tailBuffer is a concurrency-safe ring buffer retaining the trailing max bytes
// written to it.
type tailBuffer struct {
	mu   sync.Mutex
	max  int
	buf  []byte
	full bool
	pos  int
}

func newTailBuffer(max int) *tailBuffer {
	return &tailBuffer{
		max: max,
		buf: make([]byte, max),
	}
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	n := len(p)
	if n >= t.max {
		copy(t.buf, p[n-t.max:])
		t.pos = 0
		t.full = true
		return n, nil
	}

	first := min(n, t.max-t.pos)
	copy(t.buf[t.pos:], p[:first])
	if first < n {
		copy(t.buf[0:], p[first:])
	}
	t.pos = (t.pos + n) % t.max
	if t.pos == 0 || first < n {
		t.full = true
	}
	return n, nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.full {
		return string(t.buf[:t.pos])
	}
	out := make([]byte, t.max)
	n := copy(out, t.buf[t.pos:])
	copy(out[n:], t.buf[:t.pos])
	return string(out)
}

func readCapturedTaskOutput(outputFile string, tail *tailBuffer) string {
	if outputFile != "" {
		if raw, err := os.ReadFile(outputFile); err == nil {
			text := strings.TrimRight(string(raw), "\r\n")
			if text != "" {
				if len(text) > MaxOutputBytes {
					return text[:MaxOutputBytes]
				}
				return text
			}
		}
	}
	if tail == nil {
		return ""
	}
	return strings.TrimRight(tail.String(), "\r\n")
}
