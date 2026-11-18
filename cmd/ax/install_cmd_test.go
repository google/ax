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
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/ax/internal/install/axdeploy"
	"github.com/google/ax/internal/install/cluster"
	"github.com/google/ax/internal/install/executil"
)

func TestParseLocalArgs(t *testing.T) {
	def, err := parseLocalArgs([]string{"start"})
	if err != nil || def.subcmd != "start" || def.workers != 1 || def.name != cluster.DefaultName {
		t.Fatalf("expected default start flags, got %+v, %v", def, err)
	}
	if def.serverImage != axdeploy.DefaultServerImage || def.defaultTaskImage != axdeploy.DefaultTaskImage || def.runnerVolumeImage != axdeploy.DefaultRunnerVolumeImage {
		t.Fatalf("expected default serverImage=%q defaultTaskImage=%q, got %+v", axdeploy.DefaultServerImage, axdeploy.DefaultTaskImage, def)
	}

	got, err := parseLocalArgs([]string{
		"start",
		"--name", "dev",
		"--workers", "2",
		"--server-image", "ghcr.io/google/ax/ax-server:v0.1.0",
		"--default-task-image", "ghcr.io/agentexecutor/antigravity:1.5.8",
		"--runner-volume-image", "ghcr.io/google/ax/ax-runner-volume:v0.1.0",
	})
	if err != nil {
		t.Fatalf("parseLocalArgs: %v", err)
	}
	if got.subcmd != "start" || got.name != "dev" || got.workers != 2 ||
		got.serverImage != "ghcr.io/google/ax/ax-server:v0.1.0" ||
		got.defaultTaskImage != "ghcr.io/agentexecutor/antigravity:1.5.8" ||
		got.runnerVolumeImage != "ghcr.io/google/ax/ax-runner-volume:v0.1.0" {
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
	if def.serverImage != axdeploy.DefaultServerImage || def.defaultTaskImage != axdeploy.DefaultTaskImage || def.runnerVolumeImage != axdeploy.DefaultRunnerVolumeImage {
		t.Fatalf("expected default serverImage=%q defaultTaskImage=%q, got %+v", axdeploy.DefaultServerImage, axdeploy.DefaultTaskImage, def)
	}

	got, err := parseInstallArgs([]string{
		"--server-image", "ghcr.io/google/ax/ax-server:v0.1.0",
		"--default-task-image", "us-docker.pkg.dev/ax-prod/images/claude-code:v1",
		"--runner-volume-image", "us-docker.pkg.dev/ax-prod/images/ax-runner-volume:v1",
	})
	if err != nil {
		t.Fatalf("parseInstallArgs: %v", err)
	}
	if got.serverImage != "ghcr.io/google/ax/ax-server:v0.1.0" ||
		got.defaultTaskImage != "us-docker.pkg.dev/ax-prod/images/claude-code:v1" ||
		got.runnerVolumeImage != "us-docker.pkg.dev/ax-prod/images/ax-runner-volume:v1" {
		t.Fatalf("unexpected parsed flags: %+v", got)
	}

	if _, err := parseInstallArgs([]string{"unexpected"}); err == nil {
		t.Fatal("expected error on unexpected positional argument")
	}
}

func TestRunLocalAndInstallGateWhenDefaultImagesEmpty(t *testing.T) {
	t.Setenv("AX_SERVER_IMAGE", "")
	t.Setenv("AX_DEFAULT_TASK_IMAGE", "")

	if err := runLocal("ax-system", []string{"start"}); err == nil || !strings.Contains(err.Error(), "--server-image") {
		t.Fatalf("expected runLocal start to fail fast on gated server image, got: %v", err)
	}
	if err := runInstall("", "ax-system", nil); err == nil || !strings.Contains(err.Error(), "--server-image") {
		t.Fatalf("expected runInstall to fail fast on gated server image, got: %v", err)
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
current-context: ax-local
contexts:
  - name: ax-local
    context:
      cluster: kind-ax-local
      user: kind-ax-local
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
	if !strings.Contains(out, "CURRENT") || !strings.Contains(out, "ax-local") || !strings.Contains(out, "prod") {
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

func TestFormatLocalStatus(t *testing.T) {
	var absentBuf bytes.Buffer
	if err := formatLocalStatus(&absentBuf, cluster.Status{
		Name:            "N/A",
		Backend:         "kind",
		State:           cluster.StateAbsent,
		KubeContext:     "N/A",
		SubstrateStatus: "N/A",
		AXStatus:        "N/A",
	}); err != nil {
		t.Fatalf("formatLocalStatus absent: %v", err)
	}
	wantAbsent := strings.Join([]string{
		"Cluster:       N/A",
		"State:         NotFound",
		"Kube Context:  N/A",
		"Substrate:     N/A",
		"AX Server:     N/A",
		"",
	}, "\n")
	if absentBuf.String() != wantAbsent {
		t.Fatalf("unexpected absent status output:\ngot:\n%s\nwant:\n%s", absentBuf.String(), wantAbsent)
	}

	var runningBuf bytes.Buffer
	if err := formatLocalStatus(&runningBuf, cluster.Status{
		Name:            "ax-local",
		Backend:         "kind",
		State:           cluster.StateRunning,
		KubeContext:     "ax-local",
		SubstrateStatus: "ate-api-server (1/1 available)",
		AXStatus:        "ax-server (1/1 available in ax-system)",
	}); err != nil {
		t.Fatalf("formatLocalStatus running: %v", err)
	}
	if !strings.Contains(runningBuf.String(), "Cluster:       ax-local (kind)\n") ||
		!strings.Contains(runningBuf.String(), "Kube Context:  ax-local\n") {
		t.Fatalf("unexpected running status output:\n%s", runningBuf.String())
	}
}

func TestParseLocalArgsOutput(t *testing.T) {
	for _, flagName := range []string{"-o", "--output"} {
		got, err := parseLocalArgs([]string{"status", flagName, "json"})
		if err != nil || got.output != "json" {
			t.Errorf("status %s json: output = %q, error %v", flagName, got.output, err)
		}
	}
	if _, err := parseLocalArgs([]string{"status", "-o", "yaml"}); err == nil {
		t.Error("expected an error for an unsupported output format")
	}
	if _, err := parseLocalArgs([]string{"start", "-o", "json"}); err == nil {
		t.Error("expected an error for -o on a subcommand other than status")
	}
}

// hostRunner fakes the container engine CLI. With no docker binary set, no
// engine is found.
type hostRunner struct {
	docker string // path LookPath returns for "docker"
	info   string // output of `docker info --format ...`
}

func (r hostRunner) LookPath(name string) (string, error) {
	if name == "docker" && r.docker != "" {
		return r.docker, nil
	}
	return "", errors.New("not found")
}

func (r hostRunner) Run(_ context.Context, spec executil.Spec) (executil.Result, error) {
	if len(spec.Args) > 1 && spec.Args[1] == "--format" {
		return executil.Result{Stdout: r.info}, nil
	}
	return executil.Result{}, nil
}

// statusProvider returns a fixed status and records whether it was asked.
type statusProvider struct {
	cluster.Provider
	status cluster.Status
	called bool
}

func (p *statusProvider) Status(context.Context, string) (cluster.Status, error) {
	p.called = true
	return p.status, nil
}

func TestWriteLocalStatusJSON(t *testing.T) {
	t.Setenv("CONTAINER_CLI", "")

	// A running cluster on a host that passes the checks, with 5 GiB of RAM,
	// which is under the recommended 6 GiB.
	provider := &statusProvider{status: cluster.Status{
		Name:            "ax-local",
		Backend:         "kind",
		State:           cluster.StateRunning,
		KubeContext:     "ax-local",
		Nodes:           []string{"ax-local-control-plane"},
		SubstrateStatus: "ate-api-server (1/1 available)",
		AXStatus:        "ax-server (1/1 available in ax-system)",
	}}
	var out bytes.Buffer
	runner := hostRunner{docker: "/usr/bin/docker", info: "6.8.0-45-generic|5368709120|[name=seccomp]\n"}
	if err := writeLocalStatusJSON(context.Background(), &out, runner, provider, "ax-local"); err != nil {
		t.Fatal(err)
	}
	want := `{
  "name": "ax-local",
  "backend": "kind",
  "state": "Running",
  "kubeContext": "ax-local",
  "nodes": [
    "ax-local-control-plane"
  ],
  "substrateStatus": "ate-api-server (1/1 available)",
  "axStatus": "ax-server (1/1 available in ax-system)",
  "host": {
    "runtime": "docker",
    "kernelVersion": "6.8.0-45-generic",
    "memTotalBytes": 5368709120,
    "rootless": false,
    "error": "",
    "warning": "container host has 5.0 GiB RAM (\u003c 6.0 GiB recommended); pods may stay Pending under load."
  }
}
`
	if out.String() != want {
		t.Errorf("running cluster:\ngot:\n%s\nwant:\n%s", out.String(), want)
	}

	// A kernel older than 6.8 is reported in host.error; the command succeeds.
	out.Reset()
	runner.info = "5.15.0|8589934592|[]\n"
	if err := writeLocalStatusJSON(context.Background(), &out, runner, provider, "ax-local"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"error": "container host Linux kernel \"5.15.0\" is older than 6.8`) {
		t.Errorf("old kernel: host.error not reported:\n%s", out.String())
	}

	// Without a container engine the cluster is not inspected.
	out.Reset()
	provider.called = false
	if err := writeLocalStatusJSON(context.Background(), &out, hostRunner{}, provider, "ax-local"); err != nil {
		t.Fatal(err)
	}
	if provider.called {
		t.Error("no container engine: the cluster was inspected")
	}
	for _, field := range []string{`"state": "NotFound"`, `"nodes": []`, `"runtime": ""`, `"error": "no running container engine found`} {
		if !strings.Contains(out.String(), field) {
			t.Errorf("no container engine: missing %s in:\n%s", field, out.String())
		}
	}
}
