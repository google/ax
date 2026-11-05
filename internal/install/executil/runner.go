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

// Package executil wraps external CLI execution (kubectl, kind, docker/podman,
// go) behind an interface so installer steps can be unit-tested without a
// container daemon or Kubernetes cluster.
package executil

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Spec describes one external command invocation.
type Spec struct {
	Name   string
	Args   []string
	Dir    string
	Env    []string // Extra KEY=VALUE entries merged on top of os.Environ().
	Stdin  io.Reader
	Stdout io.Writer // Optional live stream in addition to captured stdout.
	Stderr io.Writer // Optional live stream in addition to captured stderr.
}

// Result holds captured output from a command invocation.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runner executes external commands and resolves binaries.
type Runner interface {
	Run(ctx context.Context, spec Spec) (Result, error)
	LookPath(name string) (string, error)
}

// OSRunner executes real OS subprocesses.
type OSRunner struct{}

// LookPath locates an executable in PATH (or returns name if it contains a slash
// and is executable).
func (OSRunner) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}

// Run executes spec and returns captured stdout/stderr.
func (OSRunner) Run(ctx context.Context, spec Spec) (Result, error) {
	cmd := exec.CommandContext(ctx, spec.Name, spec.Args...)
	if spec.Dir != "" {
		cmd.Dir = spec.Dir
	}
	if len(spec.Env) > 0 {
		cmd.Env = append(os.Environ(), spec.Env...)
	}
	if spec.Stdin != nil {
		cmd.Stdin = spec.Stdin
	}

	var outBuf, errBuf bytes.Buffer
	if spec.Stdout != nil {
		cmd.Stdout = io.MultiWriter(&outBuf, spec.Stdout)
	} else {
		cmd.Stdout = &outBuf
	}
	if spec.Stderr != nil {
		cmd.Stderr = io.MultiWriter(&errBuf, spec.Stderr)
	} else {
		cmd.Stderr = &errBuf
	}

	err := cmd.Run()
	res := Result{
		Stdout: outBuf.String(),
		Stderr: errBuf.String(),
	}
	if err != nil {
		res.ExitCode = 1
		if exitErr, ok := err.(*exec.ExitError); ok {
			res.ExitCode = exitErr.ExitCode()
		}
		detail := strings.TrimSpace(res.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(res.Stdout)
		}
		if detail != "" {
			return res, fmt.Errorf("%s %s: %w (%s)", spec.Name, strings.Join(spec.Args, " "), err, detail)
		}
		return res, fmt.Errorf("%s %s: %w", spec.Name, strings.Join(spec.Args, " "), err)
	}
	return res, nil
}
