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

package internal

import (
	"bytes"
	"testing"
)

func TestDisplay_Streaming(t *testing.T) {
	t.Run("consecutive text chunks are concatenated", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		d := NewDisplay("test-id", &buf)

		d.DisplayText("Hello ")
		d.DisplayText("world")
		d.DisplayText("!")

		got := buf.String()
		want := "Hello world!"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("consecutive thought chunks are concatenated with prefix", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		d := NewDisplay("test-id", &buf)

		d.DisplayThought("thinking ")
		d.DisplayThought("deeply")

		got := buf.String()
		want := "Thinking: thinking deeply"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("transition from thought to text adds newline", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		d := NewDisplay("test-id", &buf)

		d.DisplayThought("thinking")
		d.DisplayText("Hello")

		got := buf.String()
		want := "Thinking: thinking\nHello"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("transition from text to thought adds newline", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		d := NewDisplay("test-id", &buf)

		d.DisplayText("Hello")
		d.DisplayThought("thinking")

		got := buf.String()
		want := "Hello\nThinking: thinking"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("FinishOutput empty resets state and adds newlines", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		d := NewDisplay("test-id", &buf)

		d.DisplayText("Hello")
		d.FinishOutput("")

		got := buf.String()
		want := "Hello\n\n"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("FinishOutput with info prints info and resets state", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		d := NewDisplay("test-id", &buf)

		d.DisplayText("Hello")
		d.FinishOutput("seq=1")

		got := buf.String()
		if !bytes.HasPrefix([]byte(got), []byte("Hello\n")) {
			t.Errorf("expected Hello to end with newline, got %q", got)
		}
		if !bytes.Contains([]byte(got), []byte("seq=1")) {
			t.Errorf("expected output to contain seq=1, got %q", got)
		}
	})

	t.Run("DisplaySystem resets state and prints newline", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		d := NewDisplay("test-id", &buf)

		d.DisplayText("Hello")
		d.DisplaySystem("system message")

		got := buf.String()
		want := "Hello\nsystem message\n"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("DisplayInput resets state and adds separation newlines", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		d := NewDisplay("test-id", &buf)

		d.DisplayText("Hello")
		d.DisplayInput("prompt")

		got := buf.String()
		if !bytes.HasPrefix([]byte(got), []byte("Hello\n")) {
			t.Errorf("expected Hello to end with newline, got %q", got)
		}
		if !bytes.Contains([]byte(got), []byte("prompt")) {
			t.Errorf("expected output to contain prompt, got %q", got)
		}
	})
}
