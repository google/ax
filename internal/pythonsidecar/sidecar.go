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
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/ax/internal/config"
)

// Config holds the configuration parameters for the sidecar lifecycle.
type Config struct {
	// Module is the Python module name to run using the "-m" flag. (Required)
	Module string
	// Args contains any additional arguments to pass to the module. (Optional)
	Args []string
	// Stdout redirects the sidecar's standard output. (Optional)
	Stdout io.Writer
	// Stderr redirects the sidecar's standard error. (Optional)
	Stderr io.Writer
	// ReadyFunc is an optional function to check if the server is ready to accept requests.
	// When provided, Start will poll ReadyFunc until it returns nil or the context expires. (Optional)
	ReadyFunc func(ctx context.Context) error
	// Address is the network host:port address used for readiness checks / error reporting. (Optional)
	Address string
}

// TODO: Use /var/ax_agy_harness_service for communication instead of TCP.

// Sidecar manages the lifecycle of the underlying Python process.
type Sidecar struct {
	cfg Config

	mu       sync.Mutex
	cmd      *exec.Cmd
	pid      int
	attached bool
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

func resolvePIDFile() (string, error) {
	axDir, err := config.AXAssetsDir()
	if err != nil {
		return "", fmt.Errorf("failed to resolve AX assets dir: %w", err)
	}
	return filepath.Join(axDir, "sidecar.pid"), nil
}

func isAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil || errors.Is(err, syscall.EPERM) {
		return true
	}
	return false
}

func readPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pidStr := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid pid in file: %q", pidStr)
	}
	return pid, nil
}

func writePID(path string, pid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0644)
}

func cleanPID(expectedPID int) {
	path, err := resolvePIDFile()
	if err != nil || path == "" {
		return
	}
	pid, err := readPID(path)
	if err == nil && pid == expectedPID {
		_ = os.Remove(path)
	}
}

// Start launches the Python process (or attaches to an existing working one) and monitors its lifecycle.
// If ReadyFunc is configured, Start blocks until the server is ready or the context expires.
// If the process fails to start or become ready, an error is returned immediately.
func (s *Sidecar) Start(ctx context.Context, pythonPath string) error {
	s.mu.Lock()
	if isAlive(s.pid) {
		s.mu.Unlock()
		return fmt.Errorf("sidecar is already running")
	}

	if s.cfg.Module == "" {
		s.mu.Unlock()
		return fmt.Errorf("Module cannot be empty")
	}
	s.mu.Unlock()

	pidPath, err := resolvePIDFile()
	if err != nil {
		return err
	}

	// 1. Check if an existing PID is already running and working
	existingPID, readErr := readPID(pidPath)
	if readErr == nil && s.cfg.ReadyFunc != nil && isAlive(existingPID) {
		var readyErr error
		if s.cfg.ReadyFunc != nil {
			readyErr = s.cfg.ReadyFunc(ctx)
		}
		if readyErr == nil {
			// Existing process is alive and working!
			s.mu.Lock()
			s.pid = existingPID
			s.attached = true
			s.stopping = false
			s.exitErr = nil
			s.doneChan = make(chan struct{})
			s.mu.Unlock()

			go s.monitor()
			return nil
		}
	}

	// Existing PID is missing, dead, or not working. Clean up stale PID file.
	if readErr == nil {
		_ = os.Remove(pidPath)
	}

	if s.cfg.ReadyFunc != nil {
		if err := s.cfg.ReadyFunc(ctx); err == nil {
			return fmt.Errorf("cannot start sidecar: endpoint %s is already in use by another process", s.cfg.Address)
		}
	}

	// Prepare arguments: python -u -m module [args...]
	// -u forces unbuffered stdout/stderr so logs stream to Go instantly
	fullArgs := append([]string{"-u", "-m", s.cfg.Module}, s.cfg.Args...)

	cmd := exec.CommandContext(ctx, "python3", fullArgs...)
	if pythonPath != "" {
		env := append([]string(nil), os.Environ()...)
		var found bool
		for i, kv := range env {
			if strings.HasPrefix(kv, "PYTHONPATH=") {
				existing := strings.TrimPrefix(kv, "PYTHONPATH=")
				if existing != "" {
					env[i] = "PYTHONPATH=" + pythonPath + string(os.PathListSeparator) + existing
				} else {
					env[i] = "PYTHONPATH=" + pythonPath
				}
				found = true
				break
			}
		}
		if !found {
			env = append(env, "PYTHONPATH="+pythonPath)
		}
		cmd.Env = env
	}

	if s.cfg.Stdout != nil {
		cmd.Stdout = s.cfg.Stdout
	}
	if s.cfg.Stderr != nil {
		cmd.Stderr = s.cfg.Stderr
	}

	s.mu.Lock()
	if err := cmd.Start(); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("failed to start python process: %w", err)
	}

	s.cmd = cmd
	s.pid = cmd.Process.Pid
	s.attached = false
	s.stopping = false
	s.exitErr = nil
	s.doneChan = make(chan struct{})
	s.mu.Unlock()

	if err := writePID(pidPath, s.pid); err != nil {
		_ = s.Stop()
		return fmt.Errorf("failed to write PID file: %w", err)
	}

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

// monitor waits for the process (spawned or attached) to exit and records its exit status.
func (s *Sidecar) monitor() {
	s.mu.Lock()
	cmd := s.cmd
	pid := s.pid
	attached := s.attached
	s.mu.Unlock()

	var exitErr error
	if attached {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for {
			<-ticker.C
			s.mu.Lock()
			stopping := s.stopping
			s.mu.Unlock()
			if stopping {
				break
			}

			if !isAlive(pid) {
				exitErr = fmt.Errorf("attached python process %d exited", pid)
				break
			}
		}
	} else if cmd != nil {
		err := cmd.Wait()
		s.mu.Lock()
		stopping := s.stopping
		s.mu.Unlock()
		if err != nil && !stopping {
			exitErr = fmt.Errorf("python process exited with error: %w", err)
		}
	}

	s.mu.Lock()
	s.pid = 0
	s.exitErr = exitErr
	s.mu.Unlock()

	cleanPID(pid)
	close(s.doneChan)
}

// IsRunning returns true if the Python process is currently active.
func (s *Sidecar) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return isAlive(s.pid)
}

// Pid returns the process ID of the running sidecar, or 0 if not running.
func (s *Sidecar) Pid() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !isAlive(s.pid) {
		return 0
	}
	return s.pid
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
		pid := s.pid
		exitErr := s.exitErr
		s.mu.Unlock()
		if !isAlive(pid) {
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
	if !isAlive(s.pid) {
		s.mu.Unlock()
		return nil
	}
	s.stopping = true
	done := s.doneChan
	cmd := s.cmd
	pid := s.pid
	attached := s.attached
	s.mu.Unlock()

	defer cleanPID(pid)

	if attached {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Signal(syscall.SIGTERM)
		}
		if done != nil {
			select {
			case <-done:
			case <-time.After(1 * time.Second):
			}
		}
		return nil
	}

	if cmd == nil || cmd.Process == nil {
		return nil
	}

	// 1. Send graceful SIGTERM
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		_ = cmd.Process.Kill()
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
		_ = cmd.Process.Kill()
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



