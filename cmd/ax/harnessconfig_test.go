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

package main

import (
	"strings"
	"testing"
)

func TestNormalizeHarnessConfigJSON(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "empty", in: "", want: ""},
		{name: "whitespace clears", in: "  \n\t ", want: ""},
		{name: "valid object", in: `{"model":"gemini"}`, want: `{"model":"gemini"}`},
		{name: "trims surrounding whitespace", in: "  {\"model\":\"gemini\"}\n", want: `{"model":"gemini"}`},
		{name: "nested object", in: `{"a":{"b":1},"c":[1,2]}`, want: `{"a":{"b":1},"c":[1,2]}`},
		{name: "invalid json", in: `{bad}`, wantErr: true},
		{name: "non-object array", in: `[1,2,3]`, wantErr: true},
		{name: "non-object scalar", in: `42`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeHarnessConfigJSON(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (result %q)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
			if tt.want == "" && got != nil {
				t.Errorf("expected nil for cleared config, got %q", got)
			}
		})
	}
}

func TestPrettyHarnessConfig(t *testing.T) {
	if got := prettyHarnessConfig(nil); got != "" {
		t.Errorf("nil: got %q, want empty string", got)
	}
	if got := prettyHarnessConfig([]byte{}); got != "" {
		t.Errorf("empty: got %q, want empty string", got)
	}

	// Invalid JSON falls back to the raw bytes.
	if got := prettyHarnessConfig([]byte("not json")); got != "not json" {
		t.Errorf("invalid: got %q, want raw passthrough", got)
	}

	// Valid JSON is rendered multi-line and indented.
	got := prettyHarnessConfig([]byte(`{"model":"gemini"}`))
	if !strings.Contains(got, "model") || !strings.Contains(got, "gemini") {
		t.Errorf("valid: got %q, want it to contain the key and value", got)
	}
	if !strings.Contains(got, "\n") {
		t.Errorf("valid: got %q, want indented multi-line output", got)
	}
}
