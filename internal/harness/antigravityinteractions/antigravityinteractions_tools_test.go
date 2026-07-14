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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// numberedLines returns "1\n2\n...\nN\n".
func numberedLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%d\n", i)
	}
	return b.String()
}

func TestResolveLineWindow(t *testing.T) {
	content := "l1\nl2\nl3\nl4\nl5\n"
	tests := []struct {
		name     string
		start    int
		startSet bool
		end      int
		endSet   bool
		want     string
	}{
		{"neither set (small file)", 0, false, 0, false, "l1\nl2\nl3\nl4\nl5"},
		{"both set middle", 2, true, 4, true, "l2\nl3\nl4"},
		{"both set whole", 1, true, 5, true, "l1\nl2\nl3\nl4\nl5"},
		{"start only", 3, true, 0, false, "l3\nl4\nl5"},
		{"end only", 0, false, 3, true, "l1\nl2\nl3"},
		{"end past EOF clamps", 3, true, 999, true, "l3\nl4\nl5"},
		{"start past EOF empty", 99, true, 0, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveLineWindow(content, tt.start, tt.startSet, tt.end, tt.endSet)
			if got != tt.want {
				t.Errorf("content = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveLineWindow_StartOnlyForwardWindow(t *testing.T) {
	// start only => viewFileMaxLines lines starting at start (forward window).
	content := numberedLines(viewFileMaxLines + 500)
	got := resolveLineWindow(content, 10, true, 0, false)
	lines := strings.Split(got, "\n")
	if len(lines) != viewFileMaxLines {
		t.Fatalf("got %d lines, want forward window of %d", len(lines), viewFileMaxLines)
	}
	if lines[0] != "10" {
		t.Errorf("first line = %q, want %q (window starts at start)", lines[0], "10")
	}
}

func TestResolveLineWindow_EndOnlyBackwardWindow(t *testing.T) {
	// end only => viewFileMaxLines lines ending at end (backward window).
	total := viewFileMaxLines + 500
	content := numberedLines(total)
	end := total // last line
	got := resolveLineWindow(content, 0, false, end, true)
	lines := strings.Split(got, "\n")
	if len(lines) != viewFileMaxLines {
		t.Fatalf("got %d lines, want backward window of %d", len(lines), viewFileMaxLines)
	}
	if lines[len(lines)-1] != fmt.Sprintf("%d", end) {
		t.Errorf("last line = %q, want %q (window ends at end)", lines[len(lines)-1], fmt.Sprintf("%d", end))
	}
}

func TestResolveLineWindow_NeitherSetCapsToMaxLines(t *testing.T) {
	content := numberedLines(viewFileMaxLines + 100)
	got := resolveLineWindow(content, 0, false, 0, false)
	if n := len(strings.Split(got, "\n")); n != viewFileMaxLines {
		t.Errorf("got %d lines, want cap %d", n, viewFileMaxLines)
	}
}

func TestApplyByteWindow(t *testing.T) {
	t.Run("under cap", func(t *testing.T) {
		if got := applyByteWindow("hello", 0); got != "hello" {
			t.Errorf("got %q, want hello", got)
		}
	})
	t.Run("over cap truncates to viewFileMaxBytes", func(t *testing.T) {
		big := strings.Repeat("x", viewFileMaxBytes+100)
		if got := applyByteWindow(big, 0); len(got) != viewFileMaxBytes {
			t.Errorf("got len %d, want %d", len(got), viewFileMaxBytes)
		}
	})
	t.Run("resume via offset", func(t *testing.T) {
		big := strings.Repeat("x", viewFileMaxBytes+100)
		if got := applyByteWindow(big, viewFileMaxBytes); len(got) != 100 {
			t.Errorf("got len %d, want 100", len(got))
		}
	})
	t.Run("offset past end is empty", func(t *testing.T) {
		if got := applyByteWindow("abc", 999); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestExecViewFile_HonorsRange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\nd\ne\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := execViewFile(capturedToolCall{arguments: map[string]any{
		"AbsolutePath": path,
		"StartLine":    float64(2), // JSON numbers arrive as float64
		"EndLine":      float64(3),
	}})
	m := res.(map[string]any)
	if m["content"] != "b\nc" {
		t.Errorf("content = %q, want %q", m["content"], "b\nc")
	}
}

func TestExecViewFile_LargeFileIsCapped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.csv")
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, "row-%d-with-some-padding\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	// No range requested (the regression: previously returned the whole file).
	res := execViewFile(capturedToolCall{arguments: map[string]any{"AbsolutePath": path}})
	content := res.(map[string]any)["content"].(string)
	if len(content) > viewFileMaxBytes {
		t.Errorf("content = %d bytes, exceeds cap %d", len(content), viewFileMaxBytes)
	}
	if lines := len(strings.Split(content, "\n")); lines > viewFileMaxLines {
		t.Errorf("content = %d lines, exceeds cap %d", lines, viewFileMaxLines)
	}
}

func TestExecViewFile_ContentOffsetReadsFromOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wide.txt")
	// One long single line exceeding the byte cap.
	if err := os.WriteFile(path, []byte(strings.Repeat("z", viewFileMaxBytes+50)), 0o644); err != nil {
		t.Fatal(err)
	}
	// Default read is capped at viewFileMaxBytes.
	first := execViewFile(capturedToolCall{arguments: map[string]any{"AbsolutePath": path}})
	if got := first.(map[string]any)["content"].(string); len(got) != viewFileMaxBytes {
		t.Fatalf("first read = %d bytes, want cap %d", len(got), viewFileMaxBytes)
	}
	// Reading from ContentOffset returns the remaining bytes.
	second := execViewFile(capturedToolCall{arguments: map[string]any{
		"AbsolutePath":  path,
		"ContentOffset": float64(viewFileMaxBytes),
	}})
	if got := second.(map[string]any)["content"].(string); len(got) != 50 {
		t.Errorf("offset read = %d bytes, want 50", len(got))
	}
}

func TestExecViewFile_MissingPath(t *testing.T) {
	res := execViewFile(capturedToolCall{arguments: map[string]any{}})
	m := res.(map[string]any)
	if _, ok := m["error"]; !ok {
		t.Errorf("expected error for missing AbsolutePath, got %+v", m)
	}
}

func TestIntArgOK(t *testing.T) {
	args := map[string]any{
		"f":   float64(7),
		"i":   9,
		"s":   "12",
		"z":   float64(0),
		"bad": "nope",
	}
	cases := []struct {
		name   string
		wantN  int
		wantOK bool
	}{
		{"f", 7, true},
		{"i", 9, true},
		{"s", 12, true},
		{"z", 0, true}, // explicit 0 is present
		{"bad", 0, false},
		{"absent", 0, false},
	}
	for _, c := range cases {
		n, ok := intArgOK(args, c.name)
		if n != c.wantN || ok != c.wantOK {
			t.Errorf("intArgOK(%q) = (%d,%v), want (%d,%v)", c.name, n, ok, c.wantN, c.wantOK)
		}
	}
}
