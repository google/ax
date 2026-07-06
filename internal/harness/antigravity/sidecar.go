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

package antigravity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// Default command used to fork the Antigravity Python sidecar.
var defaultSidecarCommand = []string{"python3", "-m", "python.antigravity.harness_server"}

const (
	// reuseProbeTimeout is the fast probe deadline used to decide whether an
	// existing sidecar is already serving at the configured address. Kept short
	// so ax exec startup stays snappy when nothing is running.
	reuseProbeTimeout = 500 * time.Millisecond
	// startupTimeout is how long EnsureSidecar waits for a freshly forked
	// Python sidecar to become healthy before giving up.
	startupTimeout = 30 * time.Second
)

// SidecarConfig configures how EnsureSidecar (and SidecarCommand) fork the
// Antigravity Python sidecar. Zero values fall back to sensible defaults.
type SidecarConfig struct {
	Host string // bind host for the sidecar; defaults to "127.0.0.1"
	Port int    // bind port for the sidecar; defaults to 50053

	// Command is the argv used to fork the sidecar. Defaults to
	// {"python3", "-m", "python.antigravity.harness_server"}. --host and --port
	// are appended by SidecarCommand.
	Command []string

	// Env, Stdout, Stderr, and Stdin control the child process. If Env is nil,
	// os.Environ() is inherited. If the *put streams are nil, os.Stdout/os.Stderr
	// are used.
	Env    []string
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
}

// Sidecar tracks the state of the Antigravity harness sidecar backing an
// AntigravityHarness instance. Addr is the address clients dial. Forked is true
// iff EnsureSidecar launched the process (as opposed to reusing an existing
// one); Close() is a no-op when the sidecar was reused.
type Sidecar struct {
	Addr   string // dial address for clients
	Forked bool   // true if this sidecar was launched by EnsureSidecar

	closeOnce sync.Once
	closeErr  error
	stop      func() error // nil when the sidecar was reused
}

// Close terminates the forked sidecar if EnsureSidecar started one. Reused
// sidecars are left running.
func (s *Sidecar) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.stop != nil {
			s.closeErr = s.stop()
		}
	})
	return s.closeErr
}

// SidecarCommand returns an *exec.Cmd that runs the Antigravity Python sidecar
// per the provided SidecarConfig. It fills in defaults and wires stdio/env, but
// does not Start() the process. Used by ax harness (foreground supervisor) and
// by EnsureSidecar (auto-fork on ax exec).
func SidecarCommand(cfg SidecarConfig) *exec.Cmd {
	host := cfg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := cfg.Port
	if port == 0 {
		port = 50053
	}
	argv := cfg.Command
	if len(argv) == 0 {
		argv = defaultSidecarCommand
	}
	argv = append(argv, "--host", host, "--port", strconv.Itoa(port))

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout = cfg.Stdout
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	cmd.Stderr = cfg.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	cmd.Stdin = cfg.Stdin
	if cfg.Env != nil {
		cmd.Env = cfg.Env
	} else {
		cmd.Env = os.Environ()
	}
	setSysProcAttr(cmd) // OS-specific: e.g. Pdeathsig on Linux
	return cmd
}

// EnsureSidecar guarantees that an Antigravity sidecar is reachable at
// cfg.Host:cfg.Port when it returns nil error. It first probes the address; if
// a healthy sidecar is already serving there (e.g. because the user started
// ax harness manually or from a previous ax exec), it is reused as-is
// (Sidecar.Forked = false). Otherwise, it forks the Python sidecar and waits
// up to startupTimeout for it to become healthy.
//
// The caller is responsible for calling Sidecar.Close() when done. Close is a
// no-op for reused sidecars.
func EnsureSidecar(ctx context.Context, cfg SidecarConfig) (*Sidecar, error) {
	host := cfg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := cfg.Port
	if port == 0 {
		port = 50053
	}
	addr := fmt.Sprintf("%s:%d", host, port)

	// Fast path: if something is already answering health checks at addr, reuse
	// it. This mirrors how ax exec --server reuses an already-running controller
	// server and matches the user's mental model when they've started
	// ax harness by hand in another terminal.
	if reachable(ctx, addr, reuseProbeTimeout) {
		slog.InfoContext(ctx, "reusing existing antigravity harness", slog.String("addr", addr))
		return &Sidecar{Addr: addr, Forked: false}, nil
	}

	// Slow path: fork the sidecar and wait for it to become healthy.
	cmd := SidecarCommand(SidecarConfig{
		Host:    host,
		Port:    port,
		Command: cfg.Command,
		Env:     cfg.Env,
		Stdout:  cfg.Stdout,
		Stderr:  cfg.Stderr,
		Stdin:   cfg.Stdin,
	})
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start antigravity harness sidecar (%s): %w", strings.Join(cmd.Args, " "), err)
	}
	slog.InfoContext(ctx, "forked antigravity harness sidecar",
		slog.Int("pid", cmd.Process.Pid),
		slog.String("addr", addr),
	)

	stop := func() error {
		if cmd.Process == nil {
			return nil
		}
		// SIGTERM for a clean shutdown; the Python server's asyncio loop will
		// unwind and exit. If it's already dead, Signal returns ErrProcessDone
		// which we treat as success.
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			slog.Warn("failed to signal antigravity sidecar", slog.Any("error", err))
		}
		// Wait with a short deadline, then SIGKILL if the child is stuck.
		waitErr := make(chan error, 1)
		go func() { waitErr <- cmd.Wait() }()
		select {
		case err := <-waitErr:
			return normalizeExitErr(err)
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			return normalizeExitErr(<-waitErr)
		}
	}

	sc := &Sidecar{Addr: addr, Forked: true, stop: stop}

	if err := waitForHealthy(ctx, addr, startupTimeout); err != nil {
		// Startup failed: reap the child before returning so we don't leak.
		_ = sc.Close()
		return nil, fmt.Errorf("antigravity harness sidecar at %s did not become healthy: %w", addr, err)
	}
	return sc, nil
}

// reachable is a single-shot health probe used for the fast reuse path. It
// returns true iff the target address answers a gRPC health Check (or reports
// Unimplemented, indicating a server is reachable but does not implement the
// health service) within timeout.
func reachable(ctx context.Context, addr string, timeout time.Duration) bool {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return false
	}
	defer conn.Close()

	client := grpc_health_v1.NewHealthClient(conn)
	resp, err := client.Check(probeCtx, &grpc_health_v1.HealthCheckRequest{Service: ""})
	if err == nil && resp.GetStatus() == grpc_health_v1.HealthCheckResponse_SERVING {
		return true
	}
	if status.Code(err) == codes.Unimplemented {
		return true
	}
	return false
}

// waitForHealthy polls the gRPC health service at addr until it reports SERVING
// (or Unimplemented, meaning the port is up but the server does not implement
// the health protocol) or timeout expires. Retries with exponential backoff.
func waitForHealthy(ctx context.Context, addr string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()

	client := grpc_health_v1.NewHealthClient(conn)
	const maxBackoff = 2 * time.Second
	backoff := 100 * time.Millisecond
	for {
		resp, err := client.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: ""})
		if err == nil && resp.GetStatus() == grpc_health_v1.HealthCheckResponse_SERVING {
			return nil
		}
		if status.Code(err) == codes.Unimplemented {
			// Server is reachable but does not implement the health service;
			// treat as ready.
			return nil
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return fmt.Errorf("not healthy within %s: %w", timeout, err)
			}
			return fmt.Errorf("not healthy within %s (last status: %s)", timeout, resp.GetStatus())
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// normalizeExitErr treats SIGTERM/SIGKILL as clean shutdowns (they're expected
// when we shut the sidecar down).
func normalizeExitErr(err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			sig := ws.Signal()
			if sig == syscall.SIGTERM || sig == syscall.SIGKILL {
				return nil
			}
		}
	}
	return err
}
