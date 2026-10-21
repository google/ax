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

// Package container runs AX tasks as local containers through a
// Docker-compatible CLI. Podman is preferred: it is fully open source and
// daemonless, and on macOS and Windows it manages its own Linux VM
// (`podman machine`). Docker and nerdctl work as well since only the common
// subset of their command lines is used.
package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RuntimeEnv selects the container CLI explicitly, by name ("podman",
// "docker", "nerdctl") or path.
const RuntimeEnv = "AX_CONTAINER_RUNTIME"

// candidates are tried in order when RuntimeEnv is unset.
var candidates = []string{"podman", "docker", "nerdctl"}

// ErrNotFound is returned when a container or image does not exist.
var ErrNotFound = errors.New("not found")

// Container states as reported by `inspect --format {{.State.Status}}`.
const (
	StateCreated    = "created"
	StateRunning    = "running"
	StatePaused     = "paused"
	StateExited     = "exited"
	StateStopped    = "stopped"    // podman
	StateConfigured = "configured" // podman: created but never initialized
	StateDead       = "dead"
)

// Runtime invokes a Docker-compatible container CLI.
type Runtime struct {
	// Bin is the path of the CLI binary.
	Bin string
}

// Detect finds a container CLI, honoring RuntimeEnv. It does not check that
// the engine behind the CLI is reachable; that surfaces on first use.
func Detect() (*Runtime, error) {
	if v := os.Getenv(RuntimeEnv); v != "" {
		bin, err := exec.LookPath(v)
		if err != nil {
			return nil, fmt.Errorf("%s=%q: %w", RuntimeEnv, v, err)
		}
		return &Runtime{Bin: bin}, nil
	}
	for _, name := range candidates {
		if bin, err := exec.LookPath(name); err == nil {
			return &Runtime{Bin: bin}, nil
		}
	}
	return nil, status.Error(codes.FailedPrecondition,
		"no container runtime found: install Podman (https://podman.io, `brew install podman && podman machine init && podman machine start`) "+
			"or set "+RuntimeEnv+" to a Docker-compatible CLI")
}

// Name returns the CLI's base name, for example "podman".
func (r *Runtime) Name() string { return filepath.Base(r.Bin) }

// run executes the CLI and returns its trimmed stdout. extraEnv entries are
// appended to the current environment, so `-e NAME` flags without a value can
// hand secrets to the container without exposing them on the command line.
func (r *Runtime) run(ctx context.Context, extraEnv []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, r.Bin, args...)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", r.wrapErr(args, stderr.String(), err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// wrapErr turns a failed invocation into an error that carries the CLI's own
// message. "No such ..." becomes ErrNotFound and an unreachable engine becomes
// a gRPC Unavailable status, which the AX server treats as transient.
func (r *Runtime) wrapErr(args []string, stderr string, err error) error {
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = err.Error()
	}
	lower := strings.ToLower(msg)
	verb := ""
	if len(args) > 0 {
		verb = args[0]
	}
	switch {
	case strings.Contains(lower, "no such") || strings.Contains(lower, "not known") || strings.Contains(lower, "not found"):
		return fmt.Errorf("%s %s: %s: %w", r.Name(), verb, msg, ErrNotFound)
	case strings.Contains(lower, "cannot connect") || strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "is the docker daemon running") || strings.Contains(lower, "unable to connect to podman"):
		hint := ""
		if r.Name() == "podman" {
			hint = " (is the Podman machine running? try `podman machine start`)"
		}
		return status.Errorf(codes.Unavailable, "%s %s: %s%s", r.Name(), verb, msg, hint)
	}
	return fmt.Errorf("%s %s: %s", r.Name(), verb, msg)
}

// State returns the state of the named container, or ErrNotFound.
func (r *Runtime) State(ctx context.Context, name string) (string, error) {
	return r.run(ctx, nil, "inspect", "--type", "container", "--format", "{{.State.Status}}", name)
}

// ImageExists reports whether the image is present locally.
func (r *Runtime) ImageExists(ctx context.Context, image string) (bool, error) {
	_, err := r.run(ctx, nil, "image", "inspect", "--format", "{{.Id}}", image)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Pull fetches an image, streaming the CLI's progress output to w.
func (r *Runtime) Pull(ctx context.Context, image string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, r.Bin, "pull", image)
	var stderr bytes.Buffer
	if w == nil {
		w = io.Discard
	}
	cmd.Stdout = w
	cmd.Stderr = io.MultiWriter(w, &stderr)
	if err := cmd.Run(); err != nil {
		// The full output was already streamed to w; keep the error short.
		lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
		return r.wrapErr([]string{"pull"}, image+": "+lines[len(lines)-1], err)
	}
	return nil
}

// CreateOptions describes a container to create.
type CreateOptions struct {
	Name       string
	Image      string
	Entrypoint string
	Labels     map[string]string
	// Env is passed as NAME=VALUE flags.
	Env map[string]string
	// SecretEnv is passed through the CLI's environment and `-e NAME`, so the
	// values never appear in the process list.
	SecretEnv map[string]string
	// Publish maps container ports to ephemeral ports on 127.0.0.1.
	Publish []int
	// Volumes maps named volumes to mount paths.
	Volumes map[string]string
	// CPUs is a decimal number of cores, empty for no limit.
	CPUs string
	// MemoryBytes is the memory limit, zero for no limit.
	MemoryBytes int64
}

// Create creates (but does not start) a container.
func (r *Runtime) Create(ctx context.Context, o CreateOptions) error {
	args := []string{"create", "--name", o.Name}
	if o.Entrypoint != "" {
		args = append(args, "--entrypoint", o.Entrypoint)
	}
	for _, k := range sortedKeys(o.Labels) {
		args = append(args, "--label", k+"="+o.Labels[k])
	}
	for _, p := range o.Publish {
		args = append(args, "--publish", fmt.Sprintf("127.0.0.1::%d", p))
	}
	for _, v := range sortedKeys(o.Volumes) {
		args = append(args, "--volume", v+":"+o.Volumes[v])
	}
	if o.CPUs != "" {
		args = append(args, "--cpus", o.CPUs)
	}
	if o.MemoryBytes > 0 {
		args = append(args, "--memory", fmt.Sprintf("%db", o.MemoryBytes))
	}
	for _, k := range sortedKeys(o.Env) {
		args = append(args, "--env", k+"="+o.Env[k])
	}
	var extraEnv []string
	for _, k := range sortedKeys(o.SecretEnv) {
		args = append(args, "--env", k)
		extraEnv = append(extraEnv, k+"="+o.SecretEnv[k])
	}
	args = append(args, o.Image)
	_, err := r.run(ctx, extraEnv, args...)
	return err
}

// Start starts a created or stopped container.
func (r *Runtime) Start(ctx context.Context, name string) error {
	_, err := r.run(ctx, nil, "start", name)
	return err
}

// Pause freezes every process in a running container.
func (r *Runtime) Pause(ctx context.Context, name string) error {
	_, err := r.run(ctx, nil, "pause", name)
	return err
}

// Unpause resumes a paused container.
func (r *Runtime) Unpause(ctx context.Context, name string) error {
	_, err := r.run(ctx, nil, "unpause", name)
	return err
}

// Remove force-removes a container. A missing container is not an error.
func (r *Runtime) Remove(ctx context.Context, name string) error {
	_, err := r.run(ctx, nil, "rm", "--force", name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// RemoveVolume removes a named volume. A missing volume is not an error.
func (r *Runtime) RemoveVolume(ctx context.Context, name string) error {
	_, err := r.run(ctx, nil, "volume", "rm", "--force", name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// HostPort returns the host address ("127.0.0.1:54321") published for the
// container's TCP port.
func (r *Runtime) HostPort(ctx context.Context, name string, port int) (string, error) {
	out, err := r.run(ctx, nil, "port", name, fmt.Sprintf("%d/tcp", port))
	if err != nil {
		return "", err
	}
	// Docker may list an IPv4 and an IPv6 binding; prefer IPv4.
	lines := strings.Fields(out)
	for _, l := range lines {
		if strings.HasPrefix(l, "127.0.0.1:") || strings.HasPrefix(l, "0.0.0.0:") {
			return strings.Replace(l, "0.0.0.0:", "127.0.0.1:", 1), nil
		}
	}
	if len(lines) > 0 {
		return lines[0], nil
	}
	return "", fmt.Errorf("%s port: container %s publishes no port %d", r.Name(), name, port)
}

// Logs returns up to tail lines of the container's combined output.
func (r *Runtime) Logs(ctx context.Context, name string, tail int) string {
	cmd := exec.CommandContext(ctx, r.Bin, "logs", "--tail", fmt.Sprint(tail), name)
	out, _ := cmd.CombinedOutput()
	return strings.TrimSpace(string(out))
}
