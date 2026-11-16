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
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/google/ax/internal/install/executil"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

const studioUsage = `Usage:
  ax studio [flags]

Launch the AX Studio local Web UI in your browser (127.0.0.1 only).
Stopping this command (Ctrl-C) closes only the local web server; any local Kind
cluster and running Tasks inside Kubernetes continue running unaffected.

Flags:
  --port int       Loopback port to listen on (default: 8080)
  --addr string    Explicit loopback address (overrides --port, e.g. "127.0.0.1:8080")
  --no-open        Do not open a browser automatically (for SSH/remote sessions or scripts)`

type studioFlags struct {
	port   int
	addr   string
	noOpen bool
	help   bool
}

func parseStudioArgs(args []string) (studioFlags, error) {
	fs := flag.NewFlagSet("ax studio", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var out studioFlags
	fs.IntVar(&out.port, "port", 8080, "loopback port")
	fs.StringVar(&out.addr, "addr", "", "explicit loopback address")
	fs.BoolVar(&out.noOpen, "no-open", false, "do not open a browser automatically (for SSH/remote sessions or scripts)")
	fs.BoolVar(&out.help, "h", false, "help")
	fs.BoolVar(&out.help, "help", false, "help")

	if err := fs.Parse(args); err != nil {
		return studioFlags{}, err
	}
	if fs.NArg() > 0 {
		return studioFlags{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if !out.help {
		if _, err := resolveStudioListenAddr(out); err != nil {
			return studioFlags{}, err
		}
	}
	return out, nil
}

func resolveStudioListenAddr(parsed studioFlags) (string, error) {
	addr := strings.TrimSpace(parsed.addr)
	if addr == "" {
		if parsed.port < 0 || parsed.port > 65535 {
			return "", fmt.Errorf("invalid port %d", parsed.port)
		}
		addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(parsed.port))
	}
	if err := validateStudioLoopbackAddr(addr); err != nil {
		return "", err
	}
	return addr, nil
}

func validateStudioLoopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	if host == "" {
		return errors.New("refusing to bind AX Studio to all interfaces; specify 127.0.0.1:<port> or localhost:<port>")
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("refusing to bind AX Studio to non-loopback host %q (must be 127.0.0.1, ::1, or localhost)", host)
	}
	return nil
}

func buildStudioChildArgs(axNamespace, axExecutable string, parsed studioFlags) ([]string, error) {
	addr, err := resolveStudioListenAddr(parsed)
	if err != nil {
		return nil, err
	}
	ns := strings.TrimSpace(axNamespace)
	if ns == "" {
		ns = v1alpha1.DefaultSystemNamespace
	}
	childArgs := []string{"-n", ns, "--addr", addr}
	if strings.TrimSpace(axExecutable) != "" {
		childArgs = append(childArgs, "--ax-bin", axExecutable)
	}
	if parsed.noOpen {
		childArgs = append(childArgs, "--no-open")
	}
	return childArgs, nil
}

func runStudio(axNamespace string, args []string) error {
	parsed, err := parseStudioArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, studioUsage)
		return err
	}
	if parsed.help {
		fmt.Println(studioUsage)
		return nil
	}

	studioBin, err := executil.ResolveStudioBinary(executil.OSRunner{})
	if err != nil {
		return err
	}

	axExe, _ := os.Executable()
	childArgs, err := buildStudioChildArgs(axNamespace, axExe, parsed)
	if err != nil {
		return err
	}

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
