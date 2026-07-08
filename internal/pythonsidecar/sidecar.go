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

// Package pythonsidecar provides a mechanism to manage the lifecycle
// of a Python process as a sidecar component in a Go application.
package pythonsidecar

import (
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Config holds the configuration parameters for the sidecar lifecycle.
type Config struct {
	// Module is the Python module name to run using the "-m" flag. (Required)
	Module string
	// Args contains any additional arguments to pass to the module. (Optional)
	Args []string
	// Stdin redirects the sidecar's standard input. (Optional)
	Stdin io.Reader
	// Stdout redirects the sidecar's standard output. (Optional)
	Stdout io.Writer
	// Stderr redirects the sidecar's standard error. (Optional)
	Stderr io.Writer
	// ReadyFunc is an optional function to check if the server is ready to accept requests.
	// When provided, Start will poll ReadyFunc until it returns nil or the context expires. (Optional)
	ReadyFunc func(ctx context.Context) error
}

// TODO: AntigravityHarness should use pythonsidecar.
// TODO: Use /var/ax_agy_harness_service for communication instead of TCP.
// TODO: Use go:embed to embed python/ directory into the ax binary.
// TODO: Add a Setup method to extract embedded assets, pip install, etc.

// Sidecar manages the lifecycle of the underlying Python process.
type Sidecar struct {
	cfg Config

	mu       sync.Mutex
	cmd      *exec.Cmd
	running  bool
	stopping bool
	exitErr  error
	doneChan chan struct{}
}

// New creates a new Sidecar instance using the provided configuration struct.
func New(cfg Config) *Sidecar {
	return &Sidecar{
		cfg: cfg,
	}
}

// Start launches the Python process and monitors its lifecycle in the background.
// If ReadyFunc is configured, Start blocks until the server is ready or the context expires.
// If the process fails to start or become ready, an error is returned immediately.
func (s *Sidecar) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("sidecar is already running")
	}

	if s.cfg.Module == "" {
		s.mu.Unlock()
		return fmt.Errorf("Module cannot be empty")
	}

	// Prepare arguments: python3 -u -m module [args...]
	// -u forces unbuffered stdout/stderr so logs stream to Go instantly
	fullArgs := append([]string{"-u", "-m", s.cfg.Module}, s.cfg.Args...)

	cmd := exec.CommandContext(ctx, "python3", fullArgs...)

	if s.cfg.Stdin != nil {
		cmd.Stdin = s.cfg.Stdin
	}
	if s.cfg.Stdout != nil {
		cmd.Stdout = s.cfg.Stdout
	}
	if s.cfg.Stderr != nil {
		cmd.Stderr = s.cfg.Stderr
	}

	if err := cmd.Start(); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("failed to start python process: %w", err)
	}

	s.cmd = cmd
	s.running = true
	s.stopping = false
	s.exitErr = nil
	s.doneChan = make(chan struct{})
	s.mu.Unlock()

	// Monitor lifecycle asynchronously
	go s.monitor()

	// If a readiness probe is configured, wait for the server to become ready
	if s.cfg.ReadyFunc != nil {
		if err := s.WaitUntilReady(ctx); err != nil {
			_ = s.Stop()
			return fmt.Errorf("server failed to become ready: %w", err)
		}
	}

	return nil
}

// monitor waits for the process to exit and records its exit status.
func (s *Sidecar) monitor() {
	err := s.cmd.Wait()
	s.mu.Lock()
	s.running = false
	if err != nil && !s.stopping {
		s.exitErr = fmt.Errorf("python process exited with error: %w", err)
	} else {
		s.exitErr = nil
	}
	s.mu.Unlock()
	close(s.doneChan)
}

// IsRunning returns true if the Python process is currently active.
func (s *Sidecar) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Pid returns the process ID of the running sidecar, or 0 if not running.
func (s *Sidecar) Pid() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running || s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// WaitUntilReady blocks until ReadyFunc returns nil, the context is canceled, or the process exits prematurely.
// If no ReadyFunc is configured in Config, this method returns nil immediately.
func (s *Sidecar) WaitUntilReady(ctx context.Context) error {
	if s.cfg.ReadyFunc == nil {
		return nil
	}

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		// 1. Check if the process exited prematurely
		s.mu.Lock()
		running := s.running
		exitErr := s.exitErr
		s.mu.Unlock()
		if !running {
			if exitErr != nil {
				return fmt.Errorf("process exited before becoming ready: %w", exitErr)
			}
			return fmt.Errorf("process exited unexpectedly before becoming ready")
		}

		// 2. Try the readiness check
		if err := s.cfg.ReadyFunc(ctx); err == nil {
			return nil
		}

		// 3. Wait for the next ticker or context cancellation
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			continue
		}
	}
}

// Wait blocks until the sidecar exits or crashes, returning the exit error if any.
func (s *Sidecar) Wait() error {
	s.mu.Lock()
	done := s.doneChan
	s.mu.Unlock()
	if done != nil {
		<-done
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitErr
}

// Stop gracefully terminates the Python process using SIGTERM, falling back to SIGKILL if necessary.
func (s *Sidecar) Stop() error {
	s.mu.Lock()
	if !s.running || s.cmd == nil || s.cmd.Process == nil {
		s.mu.Unlock()
		return nil
	}
	s.stopping = true
	done := s.doneChan
	s.mu.Unlock()

	// 1. Send graceful SIGTERM
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		_ = s.cmd.Process.Kill()
		if done != nil {
			<-done
		}
		return nil
	}

	// 2. Give it a small window to exit gracefully before killing it
	select {
	case <-done:
		return nil
	case <-time.After(3 * time.Second):
		// Fallback to force kill
		_ = s.cmd.Process.Kill()
		if done != nil {
			<-done
		}
		return nil
	}
}

// TCPReady returns a ReadyFunc that attempts to establish a TCP connection to addr (e.g., "127.0.0.1:50053").
func TCPReady(addr string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		_ = conn.Close()
		return nil
	}
}
