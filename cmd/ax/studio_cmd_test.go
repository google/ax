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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseStudioArgs(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		got, err := parseStudioArgs(nil)
		if err != nil {
			t.Fatalf("parseStudioArgs(nil): %v", err)
		}
		if got.port != 8080 || got.addr != "" || got.noOpen || got.help {
			t.Fatalf("unexpected defaults: %+v", got)
		}
	})

	t.Run("rejects_non_loopback_addr", func(t *testing.T) {
		for _, bad := range []string{"0.0.0.0:8080", ":8080", "192.168.1.5:8080", "[::]:8080"} {
			if _, err := parseStudioArgs([]string{"--addr", bad}); err == nil {
				t.Errorf("expected non-loopback --addr %q to fail", bad)
			}
		}
	})

	t.Run("accepts_loopback_variants", func(t *testing.T) {
		for _, good := range []string{"127.0.0.1:9091", "localhost:8080", "[::1]:8080"} {
			if _, err := parseStudioArgs([]string{"--addr", good, "--no-open"}); err != nil {
				t.Errorf("expected loopback --addr %q to succeed, got %v", good, err)
			}
		}
	})

	t.Run("rejects_positional_arg", func(t *testing.T) {
		if _, err := parseStudioArgs([]string{"extra"}); err == nil {
			t.Fatal("expected unexpected positional argument error")
		}
	})
}

func TestBuildStudioChildArgs(t *testing.T) {
	parsed, err := parseStudioArgs([]string{"--port", "18080", "--no-open"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := buildStudioChildArgs("custom-ax", "/usr/local/bin/ax", parsed)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-n", "custom-ax", "--addr", "127.0.0.1:18080", "--ax-bin", "/usr/local/bin/ax", "--no-open"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildStudioChildArgs = %#v, want %#v", got, want)
	}
}

func TestRunStudioDelegatesToStudioBinary(t *testing.T) {
	tmp := t.TempDir()
	outFile := filepath.Join(tmp, "recorded_args.txt")
	fakeStudio := filepath.Join(tmp, "ax-studio")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + outFile + "\n"
	if err := os.WriteFile(fakeStudio, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_STUDIO_BIN", fakeStudio)

	if err := runStudio("ax-system", []string{"--port", "18088", "--no-open"}); err != nil {
		t.Fatalf("runStudio failed: %v", err)
	}

	recorded, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading recorded args: %v", err)
	}
	text := string(recorded)
	for _, expected := range []string{"-n\nax-system", "--addr\n127.0.0.1:18088", "--no-open"} {
		if !strings.Contains(text, expected) {
			t.Errorf("expected delegated args to contain %q, got:\n%s", expected, text)
		}
	}
}
