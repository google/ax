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

	"github.com/google/ax/proto"
)

func TestDisplay_Streaming(t *testing.T) {
	textContent := func(txt string) *proto.Content {
		return &proto.Content{Type: &proto.Content_Text{Text: &proto.TextContent{Text: txt}}}
	}
	thoughtContent := func(txt string) *proto.Content {
		return &proto.Content{Type: &proto.Content_Thought{Thought: &proto.ThoughtContent{
			Summary: []*proto.ThoughtSummaryContent{
				{Type: &proto.ThoughtSummaryContent_Text{Text: &proto.TextContent{Text: txt}}},
			},
		}}}
	}
	toolCallContent := func() *proto.Content {
		return &proto.Content{Type: &proto.Content_ToolCall{ToolCall: &proto.ToolCallContent{}}}
	}

	t.Run("consecutive text chunks are concatenated", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		d := NewDisplay("test-id", &buf)

		d.Display(textContent("Hello "))
		d.Display(textContent("world"))
		d.Display(textContent("!"))

		got := buf.String()
		want := "Hello world!"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("tool call separates consecutive text blocks", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		d := NewDisplay("test-id", &buf)

		d.Display(textContent("...configured."))
		d.Display(toolCallContent())
		d.Display(textContent("I will list the contents."))

		got := buf.String()
		want := "...configured.\nI will list the contents."
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("repeated tool calls do not add extra newlines", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		d := NewDisplay("test-id", &buf)

		d.Display(textContent("Done."))
		d.Display(toolCallContent())
		d.Display(toolCallContent())
		d.Display(textContent("Next."))

		got := buf.String()
		want := "Done.\nNext."
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("consecutive thought chunks are concatenated with prefix", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		d := NewDisplay("test-id", &buf)

		d.Display(thoughtContent("thinking "))
		d.Display(thoughtContent("deeply"))

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

		d.Display(thoughtContent("thinking"))
		d.Display(textContent("Hello"))

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

		d.Display(textContent("Hello"))
		d.Display(thoughtContent("thinking"))

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

		d.Display(textContent("Hello"))
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

		d.Display(textContent("Hello"))
		d.FinishOutput("seq=1")

		got := buf.String()
		if !bytes.HasPrefix([]byte(got), []byte("Hello\n")) {
			t.Errorf("expected Hello to end with newline, got %q", got)
		}
		if !bytes.Contains([]byte(got), []byte("seq=1")) {
			t.Errorf("expected output to contain seq=1, got %q", got)
		}
	})

	t.Run("displaySystem resets state and prints newline", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		d := NewDisplay("test-id", &buf)

		d.Display(textContent("Hello"))
		d.displaySystem("system message")

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

		d.Display(textContent("Hello"))
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
