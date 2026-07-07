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

// This file implements the auto-start / probe-then-fork lifecycle for the
// Antigravity Python sidecar used by `ax exec`. The mechanism is not
// AGY-specific: any harness with a co-located subprocess and a gRPC health
// probe could reuse it. TODO(#266): move Sidecar / newSidecarCmd /
// EnsureSidecar / probeIdentity / etc. into a shared package (likely
// internal/harness/autostart) so other built-in harnesses (e.g. Claude Code)
// can plug into the same machinery.

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
	// healthServiceName is the fully-qualified gRPC health service name the
	// Antigravity Python sidecar registers. Used both to reuse-probe an existing
	// sidecar and to wait for a freshly-forked sidecar's readiness. Aligns with
	// the `harnesses.antigravity` key in ax.yaml so the identity check is
	// discoverable from configuration.
	healthServiceName = "harness.antigravity"

	// reuseProbeTimeout is the fast probe deadline used to identify whether the
	// configured endpoint is already serving an Antigravity sidecar. Kept short
	// so ax exec startup stays snappy when nothing is running.
	//
	// TODO(#266): promote to ax.yaml (e.g. harnesses.antigravity.autostart.probe_timeout)
	// so both ax exec autostart and ax harness can share the same knob.
	reuseProbeTimeout = 500 * time.Millisecond
	// startupTimeout is how long EnsureSidecar waits for a freshly forked
	// Python sidecar to become healthy before giving up.
	//
	// TODO(#266): promote to ax.yaml (e.g. harnesses.antigravity.autostart.startup_timeout).
	startupTimeout = 30 * time.Second
)

// SidecarConfig configures how EnsureSidecar (and newSidecarCmd) fork the
// Antigravity Python sidecar. Zero values fall back to sensible defaults.
//
// TODO(#266): these fields should be sourced from ax.yaml
// (harnesses.antigravity.autostart.*) instead of being assembled in Go, so
// operators can tune host/port/argv/env without a rebuild and so both ax exec
// autostart and ax harness read the same knobs.
type SidecarConfig struct {
	Host string // bind host for the sidecar; defaults to "127.0.0.1"
	Port int    // bind port for the sidecar; defaults to 50053

	// Command is the argv used to fork the sidecar. Defaults to
	// {"python3", "-m", "python.antigravity.harness_server"}. --host and --port
	// are appended by newSidecarCmd.
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

// newSidecarCmd returns an *exec.Cmd that runs the Antigravity Python sidecar
// per the provided SidecarConfig. It fills in defaults and wires stdio/env, but
// does not Start() the process. Used by EnsureSidecar (auto-fork on ax exec).
//
// TODO(#265): export this and reuse from cmd/ax/harness.go:runHarness so both
// fork paths share a single source of truth for argv, stdio, env, and
// Pdeathsig. Kept private today to keep PR #251's scope minimal.
func newSidecarCmd(cfg SidecarConfig) *exec.Cmd {
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

// probeResult classifies what the caller found at the configured endpoint.
type probeResult int

const (
	// probeAGYServing means the endpoint is answering as an Antigravity sidecar
	// (gRPC health for "harness.antigravity" returned SERVING).
	probeAGYServing probeResult = iota
	// probeOtherService means the endpoint is reachable but is NOT an
	// Antigravity sidecar (it does not know the "harness.antigravity" health
	// service). Callers should treat this as user mis-config and surface a
	// clear error instead of forking on top of a foreign service.
	probeOtherService
	// probeNoService means the port is free (connection refused). Callers may
	// fork a new sidecar on this address.
	probeNoService
	// probeInconclusive means the probe could not determine the state (transport
	// error, timeout, malformed response). Callers should surface the underlying
	// error rather than making a decision.
	probeInconclusive
)

func (p probeResult) String() string {
	switch p {
	case probeAGYServing:
		return "agy-serving"
	case probeOtherService:
		return "other-service"
	case probeNoService:
		return "no-service"
	case probeInconclusive:
		return "inconclusive"
	}
	return "unknown"
}

// probeIdentity classifies the service (if any) listening at addr by asking
// the gRPC health protocol whether it is serving the Antigravity sidecar
// service name. This lets EnsureSidecar distinguish three fundamentally
// different situations: an existing sidecar to reuse, a foreign service that
// blocks us from forking, and an empty port that we can safely take.
func probeIdentity(ctx context.Context, addr string, timeout time.Duration) (probeResult, error) {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return probeInconclusive, fmt.Errorf("create gRPC client for %s: %w", addr, err)
	}
	defer conn.Close()

	client := grpc_health_v1.NewHealthClient(conn)
	resp, err := client.Check(probeCtx, &grpc_health_v1.HealthCheckRequest{Service: healthServiceName})
	if err == nil {
		if resp.GetStatus() == grpc_health_v1.HealthCheckResponse_SERVING {
			return probeAGYServing, nil
		}
		// The server responded but is not SERVING this service. Treat as
		// another service (e.g. our sidecar's status flipped to NOT_SERVING
		// during shutdown, or an unrelated gRPC service that happens to know
		// the health protocol but not our service name).
		return probeOtherService, nil
	}
	switch status.Code(err) {
	case codes.NotFound:
		// gRPC health protocol says NOT_FOUND for an unknown service name;
		// this is the canonical "some other gRPC server is here" signal.
		return probeOtherService, nil
	case codes.Unimplemented:
		// The server does not implement the health protocol at all; almost
		// certainly not our sidecar.
		return probeOtherService, nil
	case codes.Unavailable:
		// Transport error. In the "connection refused" case this means the
		// port is empty. Distinguish from other transient Unavailable errors
		// (which we surface as inconclusive) by looking at the message.
		if isConnectionRefused(err) {
			return probeNoService, nil
		}
		return probeInconclusive, err
	default:
		return probeInconclusive, err
	}
}

// isConnectionRefused reports whether a gRPC Unavailable error is caused by a
// TCP connect refusal (which for our purposes means the port is empty and can
// be taken by a fresh fork). gRPC does not expose the underlying syscall
// error typed, so we string-match; kept intentionally narrow to avoid
// misclassifying real transport failures as empty ports.
func isConnectionRefused(err error) bool {
	return strings.Contains(err.Error(), "connect: connection refused")
}

// EnsureSidecar guarantees that an Antigravity sidecar is reachable at
// cfg.Host:cfg.Port when it returns nil error. It probes the endpoint to
// identify what (if anything) is already running:
//
// TODO(#266): the SidecarConfig fields (Host, Port, Command, Env) should be
// sourced from ax.yaml (harnesses.antigravity.autostart.*) rather than
// constructed ad-hoc by cliutil.autoStartAntigravitySidecar. That would let
// operators tune argv, env, host/port without touching Go code, and lets ax
// harness share the same config.
//
//   - Existing AGY sidecar → reuse as-is (Sidecar.Forked = false, Close is a
//     no-op so the user keeps ownership of the process they started).
//   - Some other service → return an explicit error asking the user to update
//     harnesses.antigravity.endpoint in ax.yaml. We do NOT fork on top of a
//     foreign service.
//   - Empty port → fork the Python sidecar and wait for it to become healthy.
//
// The caller is responsible for calling Sidecar.Close() when done.
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

	result, probeErr := probeIdentity(ctx, addr, reuseProbeTimeout)
	switch result {
	case probeAGYServing:
		slog.InfoContext(ctx, "reusing existing antigravity harness", slog.String("addr", addr))
		return &Sidecar{Addr: addr, Forked: false}, nil

	case probeOtherService:
		return nil, fmt.Errorf(
			"antigravity harness: port %s is in use but does not respond as an "+
				"Antigravity harness (gRPC health service %q was not SERVING). "+
				"Update `harnesses.antigravity.endpoint` in ax.yaml to a free port.",
			addr, healthServiceName)

	case probeInconclusive:
		return nil, fmt.Errorf("antigravity harness: probe %s failed: %w", addr, probeErr)

	case probeNoService:
		// fall through to fork
	}

	return forkSidecar(ctx, cfg, host, port, addr)
}

// forkSidecar starts the Python sidecar via newSidecarCmd and waits for it to
// become healthy. Called only when probeIdentity classified the endpoint as
// empty (probeNoService).
func forkSidecar(ctx context.Context, cfg SidecarConfig, host string, port int, addr string) (*Sidecar, error) {
	cmd := newSidecarCmd(SidecarConfig{
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

// waitForHealthy polls the gRPC health service at addr until the Antigravity
// sidecar reports SERVING under its fully-qualified service name, or timeout
// expires. Retries with exponential backoff.
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
		resp, err := client.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: healthServiceName})
		if err == nil && resp.GetStatus() == grpc_health_v1.HealthCheckResponse_SERVING {
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
