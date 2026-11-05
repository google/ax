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
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLocalArgs(t *testing.T) {
	got, err := parseLocalArgs([]string{"up", "--name", "dev", "--workers", "1", "--from-source", "."})
	if err != nil {
		t.Fatalf("parseLocalArgs: %v", err)
	}
	if got.subcmd != "up" || got.name != "dev" || got.workers != 1 || got.fromSource != "." {
		t.Fatalf("unexpected parsed flags: %+v", got)
	}

	helpFlags, err := parseLocalArgs(nil)
	if err != nil || !helpFlags.help {
		t.Fatalf("expected help on empty args, got %+v, %v", helpFlags, err)
	}

	if _, err := parseLocalArgs([]string{"up", "extra-arg"}); err == nil {
		t.Fatal("expected error on extra positional argument")
	}
}

func TestParseInstallArgs(t *testing.T) {
	got, err := parseInstallArgs([]string{"--from-source", "/repo", "--image-repo", "localhost:5001", "--image-tag", "v0.1.0"})
	if err != nil {
		t.Fatalf("parseInstallArgs: %v", err)
	}
	if got.fromSource != "/repo" || got.imageRepo != "localhost:5001" || got.imageTag != "v0.1.0" {
		t.Fatalf("unexpected parsed flags: %+v", got)
	}

	if _, err := parseInstallArgs([]string{"unexpected"}); err == nil {
		t.Fatal("expected error on unexpected positional argument")
	}
}

func TestRunUninstallRejectsUnexpectedArgs(t *testing.T) {
	if err := runUninstall("", "ax-system", []string{"--help"}); err != nil {
		t.Fatalf("expected --help to succeed, got %v", err)
	}
	if err := runUninstall("", "ax-system", []string{"extra"}); err == nil {
		t.Fatal("expected error on unexpected positional argument")
	}
}

func TestRunContextCommand(t *testing.T) {
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	initial := `apiVersion: v1
kind: Config
current-context: local
contexts:
  - name: local
    context:
      cluster: kind-local
      user: kind-local
  - name: prod
    context:
      cluster: gke-prod
      user: gke-prod
`
	if err := os.WriteFile(kc, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kc)
	t.Setenv("KUBECONTEXT", "")

	var buf bytes.Buffer
	if err := runContextCommand("", nil, &buf); err != nil {
		t.Fatalf("runContextCommand list: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "CURRENT") || !strings.Contains(out, "local") || !strings.Contains(out, "prod") {
		t.Fatalf("unexpected context table output:\n%s", out)
	}

	buf.Reset()
	if err := runContextCommand("", []string{"prod"}, &buf); err != nil {
		t.Fatalf("runContextCommand switch: %v", err)
	}
	if !strings.Contains(buf.String(), `Switched to context "prod"`) {
		t.Fatalf("unexpected switch output: %q", buf.String())
	}

	if err := runContextCommand("", []string{"missing"}, &buf); err == nil {
		t.Fatal("expected error switching to missing context")
	}
}
