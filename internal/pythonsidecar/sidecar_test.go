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
