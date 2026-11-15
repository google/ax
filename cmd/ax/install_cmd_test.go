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

	"github.com/google/ax/internal/install/axdeploy"
)

func TestParseLocalArgs(t *testing.T) {
	def, err := parseLocalArgs([]string{"start"})
	if err != nil || def.subcmd != "start" || def.workers != 1 {
		t.Fatalf("expected default start flags (workers=1), got %+v, %v", def, err)
	}
	if def.serverImage != axdeploy.DefaultServerImage || def.defaultTaskImage != axdeploy.DefaultTaskImage {
		t.Fatalf("expected default serverImage=%q defaultTaskImage=%q, got %+v", axdeploy.DefaultServerImage, axdeploy.DefaultTaskImage, def)
	}

	got, err := parseLocalArgs([]string{
		"start",
		"--name", "dev",
		"--workers", "2",
		"--server-image", "ghcr.io/google/ax/ax-server:v0.1.0",
		"--default-task-image", "ghcr.io/agentexecutor/antigravity:1.5.8",
	})
	if err != nil {
		t.Fatalf("parseLocalArgs: %v", err)
	}
	if got.subcmd != "start" || got.name != "dev" || got.workers != 2 ||
		got.serverImage != "ghcr.io/google/ax/ax-server:v0.1.0" ||
		got.defaultTaskImage != "ghcr.io/agentexecutor/antigravity:1.5.8" {
		t.Fatalf("unexpected parsed flags: %+v", got)
	}

	helpFlags, err := parseLocalArgs(nil)
	if err != nil || !helpFlags.help {
		t.Fatalf("expected help on empty args, got %+v, %v", helpFlags, err)
	}

	if _, err := parseLocalArgs([]string{"start", "extra-arg"}); err == nil {
		t.Fatal("expected error on extra positional argument")
	}
}

func TestParseInstallArgs(t *testing.T) {
	def, err := parseInstallArgs(nil)
	if err != nil {
		t.Fatalf("parseInstallArgs defaults: %v", err)
	}
	if def.serverImage != axdeploy.DefaultServerImage || def.defaultTaskImage != axdeploy.DefaultTaskImage {
		t.Fatalf("expected default serverImage=%q defaultTaskImage=%q, got %+v", axdeploy.DefaultServerImage, axdeploy.DefaultTaskImage, def)
	}

	got, err := parseInstallArgs([]string{
		"--server-image", "ghcr.io/google/ax/ax-server:v0.1.0",
		"--default-task-image", "us-docker.pkg.dev/ax-prod/images/claude-code:v1",
	})
	if err != nil {
		t.Fatalf("parseInstallArgs: %v", err)
	}
	if got.serverImage != "ghcr.io/google/ax/ax-server:v0.1.0" ||
		got.defaultTaskImage != "us-docker.pkg.dev/ax-prod/images/claude-code:v1" {
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

func TestRunContext(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AX_HOME", filepath.Join(tmp, "ax"))
	kc := filepath.Join(tmp, "kubeconfig")
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
	if err := runContext("", nil, &buf); err != nil {
		t.Fatalf("runContext list: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "CURRENT") || !strings.Contains(out, "local") || !strings.Contains(out, "prod") {
		t.Fatalf("unexpected context table output:\n%s", out)
	}

	buf.Reset()
	if err := runContext("", []string{"prod"}, &buf); err != nil {
		t.Fatalf("runContext switch: %v", err)
	}
	if !strings.Contains(buf.String(), `Switched to context "prod"`) {
		t.Fatalf("unexpected switch output: %q", buf.String())
	}

	if err := runContext("", []string{"missing"}, &buf); err == nil {
		t.Fatal("expected error switching to missing context")
	}
}
