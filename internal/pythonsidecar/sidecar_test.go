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

package pythonsidecar_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/ax/internal/pythonsidecar"
)

func getFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to get free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func TestSidecar_ConfigValidation(t *testing.T) {
	ctx := context.Background()

	t.Run("empty Module", func(t *testing.T) {
		s := pythonsidecar.New(pythonsidecar.Config{})
		err := s.Start(ctx, "")
		if err == nil || !strings.Contains(err.Error(), "Module cannot be empty") {
			t.Fatalf("expected error about empty Module, got %v", err)
		}
	})
}

func TestSidecar_ModuleExecution(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("AX_DURABLE_DIR", tmpDir)
	modulePath := filepath.Join(tmpDir, "test_module.py")
	moduleContent := `
import sys
print("hello stdout")
print("hello stderr", file=sys.stderr)
sys.exit(0)
`
	if err := os.WriteFile(modulePath, []byte(moduleContent), 0644); err != nil {
		t.Fatalf("failed to write module: %v", err)
	}

	var stdout, stderr bytes.Buffer
	cfg := pythonsidecar.Config{
		Module: "test_module",
		Stdout: &stdout,
		Stderr: &stderr,
	}

	s := pythonsidecar.New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.Start(ctx, tmpDir); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	if err := s.Wait(); err != nil {
		t.Fatalf("Wait() failed: %v", err)
	}

	if !strings.Contains(stdout.String(), "hello stdout") {
		t.Errorf("stdout expected 'hello stdout', got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "hello stderr") {
		t.Errorf("stderr expected 'hello stderr', got %q", stderr.String())
	}
	if s.IsRunning() {
		t.Errorf("expected IsRunning() to be false after exit")
	}
}

func TestSidecar_ModuleServerWithTCPReady(t *testing.T) {
	t.Setenv("AX_DURABLE_DIR", t.TempDir())
	port := getFreePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	cfg := pythonsidecar.Config{
		Module:    "http.server",
		Args:      []string{strconv.Itoa(port), "--bind", "127.0.0.1"},
		ReadyFunc: pythonsidecar.TCPReady(addr),
	}

	s := pythonsidecar.New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s.Start(ctx, ""); err != nil {
		t.Fatalf("Start() with TCPReady failed: %v", err)
	}

	if !s.IsRunning() {
		t.Fatalf("expected server sidecar to be running")
	}
	if s.Pid() <= 0 {
		t.Fatalf("expected valid PID, got %d", s.Pid())
	}

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop() failed: %v", err)
	}
}

func TestSidecar_ReadinessFailureOnPrematureExit(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("AX_DURABLE_DIR", tmpDir)
	modulePath := filepath.Join(tmpDir, "crash.py")
	if err := os.WriteFile(modulePath, []byte("import sys; sys.exit(1)\n"), 0644); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}

	t.Setenv("PYTHONPATH", tmpDir)
	port := getFreePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	cfg := pythonsidecar.Config{
		Module:    "crash",
		ReadyFunc: pythonsidecar.TCPReady(addr),
	}

	s := pythonsidecar.New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := s.Start(ctx, "")
	if err == nil {
		t.Fatalf("expected Start() to fail when process exits prematurely")
	}
	if !strings.Contains(err.Error(), "exited before becoming ready") {
		t.Fatalf("expected 'exited before becoming ready' in error, got: %v", err)
	}
	if s.IsRunning() {
		t.Fatalf("expected IsRunning() to be false")
	}
}

func TestSidecar_EndpointAlreadyInUse(t *testing.T) {
	t.Setenv("AX_DURABLE_DIR", t.TempDir())
	// Start a dummy TCP listener on a free port
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer l.Close()
	addr := l.Addr().String()

	cfg := pythonsidecar.Config{
		Module:    "http.server",
		ReadyFunc: pythonsidecar.TCPReady(addr),
	}

	s := pythonsidecar.New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err = s.Start(ctx, "")
	if err == nil {
		t.Fatalf("expected Start() to fail when endpoint is already in use")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("expected 'already in use' in error, got: %v", err)
	}
}

func TestSidecar_PIDFileHandling(t *testing.T) {
	durableDir := t.TempDir()
	t.Setenv("AX_DURABLE_DIR", durableDir)
	pidFile := filepath.Join(durableDir, ".ax", "sidecar.pid")

	port := getFreePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	cfg1 := pythonsidecar.Config{
		Module:    "http.server",
		Args:      []string{strconv.Itoa(port), "--bind", "127.0.0.1"},
		ReadyFunc: pythonsidecar.TCPReady(addr),
	}

	s1 := pythonsidecar.New(cfg1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s1.Start(ctx, ""); err != nil {
		t.Fatalf("s1.Start() failed: %v", err)
	}

	// 1. Verify PID file was written in AX config directory
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("failed to read PID file at %s: %v", pidFile, err)
	}
	filePID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || filePID != s1.Pid() {
		t.Fatalf("PID file content mismatch: got %d, expected %d", filePID, s1.Pid())
	}

	// 2. Verify second sidecar instance adopts the working PID from PID file
	cfg2 := pythonsidecar.Config{
		Module:    "http.server",
		Args:      []string{strconv.Itoa(port), "--bind", "127.0.0.1"},
		ReadyFunc: pythonsidecar.TCPReady(addr),
	}
	s2 := pythonsidecar.New(cfg2)
	if err := s2.Start(ctx, ""); err != nil {
		t.Fatalf("s2.Start() failed to attach to working PID: %v", err)
	}

	if s2.Pid() != s1.Pid() {
		t.Fatalf("s2 PID %d does not match s1 working PID %d", s2.Pid(), s1.Pid())
	}

	// Stop s1
	if err := s1.Stop(); err != nil {
		t.Fatalf("s1.Stop() failed: %v", err)
	}

	// 3. Test dead PID cleanup
	if err := os.MkdirAll(filepath.Dir(pidFile), 0755); err != nil {
		t.Fatalf("failed to create dir for dead PID file: %v", err)
	}
	if err := os.WriteFile(pidFile, []byte("999999\n"), 0644); err != nil {
		t.Fatalf("failed to write dead PID file: %v", err)
	}

	port2 := getFreePort(t)
	addr2 := fmt.Sprintf("127.0.0.1:%d", port2)
	cfg3 := pythonsidecar.Config{
		Module:    "http.server",
		Args:      []string{strconv.Itoa(port2), "--bind", "127.0.0.1"},
		ReadyFunc: pythonsidecar.TCPReady(addr2),
	}
	s3 := pythonsidecar.New(cfg3)
	if err := s3.Start(ctx, ""); err != nil {
		t.Fatalf("s3.Start() failed when replacing dead PID: %v", err)
	}
	defer s3.Stop()

	if s3.Pid() == 999999 {
		t.Fatalf("s3 should have started a new process, but got dead PID 999999")
	}

	newData, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("failed to read updated PID file: %v", err)
	}
	newPID, _ := strconv.Atoi(strings.TrimSpace(string(newData)))
	if newPID != s3.Pid() {
		t.Fatalf("updated PID file mismatch: got %d, expected %d", newPID, s3.Pid())
	}
}

