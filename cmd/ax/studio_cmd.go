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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/google/ax/internal/install/executil"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

func hasStudioArgFlag(args []string, names ...string) bool {
	for _, a := range args {
		if a == "--" {
			break
		}
		for _, n := range names {
			if a == n || strings.HasPrefix(a, n+"=") {
				return true
			}
		}
	}
	return false
}

// buildStudioChildArgs constructs the argv forwarded verbatim to `ax-studio`.
// `ax` intentionally does not parse or mirror `ax-studio` flags so the two
// repositories stay loosely coupled as `ax-studio` evolves.
func buildStudioChildArgs(axNamespace, axExecutable string, args []string) []string {
	var childArgs []string
	if !hasStudioArgFlag(args, "-n", "--namespace") {
		ns := strings.TrimSpace(axNamespace)
		if ns == "" {
			ns = v1alpha1.DefaultSystemNamespace
		}
		childArgs = append(childArgs, "-n", ns)
	}
	if strings.TrimSpace(axExecutable) != "" && !hasStudioArgFlag(args, "--ax-bin") {
		childArgs = append(childArgs, "--ax-bin", axExecutable)
	}
	if len(args) == 1 && args[0] == "help" {
		return append(childArgs, "--help")
	}
	return append(childArgs, args...)
}

func runStudio(axNamespace string, args []string) error {
	studioBin, err := executil.ResolveStudioBinary(executil.OSRunner{})
	if err != nil {
		return err
	}

	axExe, _ := os.Executable()
	childArgs := buildStudioChildArgs(axNamespace, axExe, args)

	cmd := exec.Command(studioBin, childArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launching %s: %w", studioBin, err)
	}

	// Ignore SIGINT in the parent wrapper (the TTY already delivers Ctrl-C to the
	// child `ax-studio` in the foreground process group so it can drain HTTP
	// requests and close web terminal pipes gracefully without receiving a
	// duplicate signal), and forward explicit SIGTERM sent to the parent PID.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- cmd.Wait()
	}()

	for {
		select {
		case sig := <-sigCh:
			if sig == syscall.SIGTERM && cmd.Process != nil {
				_ = cmd.Process.Signal(syscall.SIGTERM)
			}
		case waitErr := <-doneCh:
			if waitErr != nil {
				var exitErr *exec.ExitError
				if errors.As(waitErr, &exitErr) && exitErr.ExitCode() == 130 {
					return nil
				}
			}
			return waitErr
		}
	}
}
