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
	"unicode/utf8"
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
			got, _, _, _ := resolveLineWindow(content, tt.start, tt.startSet, tt.end, tt.endSet)
			if got != tt.want {
				t.Errorf("content = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveLineWindow_StartOnlyForwardWindow(t *testing.T) {
	// start only => viewFileMaxLines lines starting at start (forward window).
	content := numberedLines(viewFileMaxLines + 500)
	got, _, _, _ := resolveLineWindow(content, 10, true, 0, false)
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
	got, _, _, _ := resolveLineWindow(content, 0, false, end, true)
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
	got, _, _, _ := resolveLineWindow(content, 0, false, 0, false)
	if n := len(strings.Split(got, "\n")); n != viewFileMaxLines {
		t.Errorf("got %d lines, want cap %d", n, viewFileMaxLines)
	}
}

func TestApplyByteWindow(t *testing.T) {
	// truncated reports whether applyByteWindow(content, offset)==out capped the
	// window, i.e. there is still content past the returned slice. Callers derive
	// this the same way from the wire fields (offset+len(content) <
	// line_range_bytes).
	truncated := func(content string, offset, gotLen int) bool {
		return offset+gotLen < len(content)
	}

	t.Run("under cap: not truncated", func(t *testing.T) {
		got := applyByteWindow("hello", 0)
		if got != "hello" || truncated("hello", 0, len(got)) {
			t.Errorf("got %q (truncated=%v), want hello (false)", got, truncated("hello", 0, len(got)))
		}
	})
	t.Run("over cap truncates to viewFileMaxBytes + resumes at next", func(t *testing.T) {
		big := strings.Repeat("x", viewFileMaxBytes+100)
		got := applyByteWindow(big, 0)
		if len(got) != viewFileMaxBytes || !truncated(big, 0, len(got)) {
			t.Errorf("got len %d (truncated=%v), want %d (true)", len(got), truncated(big, 0, len(got)), viewFileMaxBytes)
		}
		// Resume offset is offset+len(got); the remainder is the trailing 100 bytes.
		if next := len(got); next != viewFileMaxBytes {
			t.Errorf("resume offset = %d, want %d", next, viewFileMaxBytes)
		}
	})
	t.Run("resume via offset returns remainder", func(t *testing.T) {
		big := strings.Repeat("x", viewFileMaxBytes+100)
		got := applyByteWindow(big, viewFileMaxBytes)
		if len(got) != 100 || truncated(big, viewFileMaxBytes, len(got)) {
			t.Errorf("got len %d (truncated=%v), want 100 (false)", len(got), truncated(big, viewFileMaxBytes, len(got)))
		}
	})
	t.Run("offset past end is empty", func(t *testing.T) {
		got := applyByteWindow("abc", 999)
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("caps at valid UTF-8 boundary (no split rune)", func(t *testing.T) {
		// Fill just under the cap with ASCII, then place a 3-byte rune ("€" is
		// U+20AC, 3 bytes) straddling the cap so a naive byte cut would split it.
		const r = "€"               // 3 bytes: E2 82 AC
		pad := viewFileMaxBytes - 1 // cap lands 1 byte into the rune
		content := strings.Repeat("a", pad) + r + "tail"
		got := applyByteWindow(content, 0)
		if !truncated(content, 0, len(got)) {
			t.Fatal("expected truncation")
		}
		if !utf8.ValidString(got) {
			t.Errorf("returned content is not valid UTF-8 (split rune): last bytes %x", got[len(got)-3:])
		}
		// The rune must not be partially included: length backs off to pad.
		if len(got) != pad {
			t.Errorf("got len %d, want %d (backed off before the straddling rune)", len(got), pad)
		}
		// Resuming from offset+len(got) yields the rune intact at the front.
		rest := applyByteWindow(content, len(got))
		if !strings.HasPrefix(rest, r) {
			t.Errorf("resume did not start at the rune boundary: %.6q", rest)
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

func TestExecViewFile_PaginationMetadataAndResume(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wide.txt")
	totalBytes := viewFileMaxBytes + 50
	// One long single line exceeding the byte cap.
	if err := os.WriteFile(path, []byte(strings.Repeat("z", totalBytes)), 0o644); err != nil {
		t.Fatal(err)
	}
	// First read: capped at viewFileMaxBytes; reports pagination metadata so the
	// converter can detect byte truncation (content_offset+len(content) <
	// line_range_bytes).
	first := execViewFile(capturedToolCall{arguments: map[string]any{"AbsolutePath": path}}).(map[string]any)
	if got := first["content"].(string); len(got) != viewFileMaxBytes {
		t.Fatalf("first read = %d bytes, want cap %d", len(got), viewFileMaxBytes)
	}
	if first["content_offset"] != 0 {
		t.Errorf("content_offset = %v, want 0", first["content_offset"])
	}
	if first["line_range_bytes"] != totalBytes {
		t.Errorf("line_range_bytes = %v, want %d (pre-truncation total)", first["line_range_bytes"], totalBytes)
	}
	// Byte-truncation detectable: offset + len(content) < line_range_bytes.
	if got := first["content_offset"].(int) + len(first["content"].(string)); got >= first["line_range_bytes"].(int) {
		t.Errorf("offset+len = %d, want < line_range_bytes %d (should look truncated)", got, first["line_range_bytes"])
	}

	// Resume from where the first slice ended: returns the remaining 50 bytes.
	resumeOffset := len(first["content"].(string)) // 0 + viewFileMaxBytes
	second := execViewFile(capturedToolCall{arguments: map[string]any{
		"AbsolutePath":  path,
		"ContentOffset": float64(resumeOffset),
	}}).(map[string]any)
	if got := second["content"].(string); len(got) != 50 {
		t.Errorf("resume read = %d bytes, want 50", len(got))
	}
	// Now complete: offset + len == line_range_bytes.
	if got := second["content_offset"].(int) + len(second["content"].(string)); got != second["line_range_bytes"].(int) {
		t.Errorf("resume offset+len = %d, want == line_range_bytes %d (complete)", got, second["line_range_bytes"])
	}
}

func TestExecViewFile_MultiByteRuneStraddlesByteCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wide_utf8.txt")
	// One long single line: ASCII padding so the byte cap lands 1 byte into a
	// 3-byte rune ("€" is U+20AC, E2 82 AC), followed by more content so the read
	// is byte-truncated and must continue.
	const r = "€"
	pad := viewFileMaxBytes - 1
	content := strings.Repeat("a", pad) + r + strings.Repeat("b", 40)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// First page: capped, backed off before the straddling rune -> valid UTF-8.
	first := execViewFile(capturedToolCall{arguments: map[string]any{"AbsolutePath": path}}).(map[string]any)
	firstContent := first["content"].(string)
	if !utf8.ValidString(firstContent) {
		t.Errorf("first page is not valid UTF-8 (split rune): last bytes %x", firstContent[len(firstContent)-3:])
	}
	if len(firstContent) != pad {
		t.Errorf("first page = %d bytes, want %d (backed off before the straddling rune)", len(firstContent), pad)
	}
	// Byte-truncation detectable: content_offset+len(content) < line_range_bytes.
	if got := first["content_offset"].(int) + len(firstContent); got >= first["line_range_bytes"].(int) {
		t.Errorf("offset+len = %d, want < line_range_bytes %d (should look truncated)", got, first["line_range_bytes"])
	}

	// Second page: resume from where the first ended. The rune must appear intact
	// at the front and the page must be valid UTF-8.
	resumeOffset := first["content_offset"].(int) + len(firstContent)
	second := execViewFile(capturedToolCall{arguments: map[string]any{
		"AbsolutePath":  path,
		"ContentOffset": float64(resumeOffset),
	}}).(map[string]any)
	secondContent := second["content"].(string)
	if !utf8.ValidString(secondContent) {
		t.Errorf("resume page is not valid UTF-8: first bytes %x", secondContent[:3])
	}
	if !strings.HasPrefix(secondContent, r) {
		t.Errorf("resume page did not start at the rune boundary: %.6q", secondContent)
	}
	// Now complete: offset + len == line_range_bytes.
	if got := second["content_offset"].(int) + len(secondContent); got != second["line_range_bytes"].(int) {
		t.Errorf("resume offset+len = %d, want == line_range_bytes %d (complete)", got, second["line_range_bytes"])
	}
}

func TestExecViewFile_CompleteReadMetadata(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "small.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := execViewFile(capturedToolCall{arguments: map[string]any{"AbsolutePath": path}}).(map[string]any)
	// 3 lines, served whole: start_line=0, end_line=2 (0-indexed inclusive).
	if res["start_line"] != 0 || res["end_line"] != 2 {
		t.Errorf("served range = (%v,%v), want (0,2)", res["start_line"], res["end_line"])
	}
	if res["num_lines"] != 3 {
		t.Errorf("num_lines = %v, want 3", res["num_lines"])
	}
	// Complete (not byte-truncated): content_offset+len(content) == line_range_bytes.
	if got := res["content_offset"].(int) + len(res["content"].(string)); got != res["line_range_bytes"].(int) {
		t.Errorf("offset+len = %d, want == line_range_bytes %d (complete read)", got, res["line_range_bytes"])
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
