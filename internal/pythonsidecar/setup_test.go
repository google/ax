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
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/ax/internal/pythonsidecar"
	"github.com/google/ax/python"
)

func TestSetup_EmbeddedFS(t *testing.T) {
	if _, err := fs.Stat(python.FS, "antigravity/__pycache__"); err == nil {
		t.Errorf("expected antigravity/__pycache__ to be ignored when embedding, but it was found")
	}

	targetDir := filepath.Join(t.TempDir(), "target")
	opts := pythonsidecar.SetupOptions{
		FS:        python.FS,
		TargetDir: targetDir,
	}

	gotDir, err := pythonsidecar.Setup(context.Background(), opts)
	if err != nil {
		t.Fatalf("Setup() failed: %v", err)
	}
	if gotDir != targetDir {
		t.Errorf("expected targetDir=%q, got %q", targetDir, gotDir)
	}

	// Verify files were extracted under TargetDir/python
	harnessPath := filepath.Join(targetDir, "python", "antigravity", "harness_server.py")
	if _, err := os.Stat(harnessPath); err != nil {
		t.Errorf("expected file %s to exist, got stat error: %v", harnessPath, err)
	}
	protoPath := filepath.Join(targetDir, "python", "proto", "ax_pb2.py")
	if _, err := os.Stat(protoPath); err != nil {
		t.Errorf("expected file %s to exist, got stat error: %v", protoPath, err)
	}

	// Verify that subsequent Setup calls when TargetDir exists succeed without re-extracting
	if _, err := pythonsidecar.Setup(context.Background(), opts); err != nil {
		t.Fatalf("subsequent Setup() failed when TargetDir exists: %v", err)
	}
}

func TestSidecar_Setup(t *testing.T) {
	tmpDir := filepath.Join(t.TempDir(), "target")
	s := pythonsidecar.New(pythonsidecar.Config{
		Module: "test_module",
	})

	err := s.Setup(context.Background(), pythonsidecar.SetupOptions{
		FS:        python.FS,
		TargetDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("Sidecar.Setup() failed: %v", err)
	}

	// Verify Sidecar cannot run Setup while already running
	modulePath := filepath.Join(tmpDir, "test_module.py")
	if err := os.WriteFile(modulePath, []byte("import time\ntime.sleep(2)\n"), 0644); err != nil {
		t.Fatalf("failed to write test module: %v", err)
	}
	t.Setenv("PYTHONPATH", tmpDir)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	if err := s.Setup(ctx, pythonsidecar.SetupOptions{FS: python.FS, TargetDir: tmpDir}); err == nil {
		t.Errorf("expected Sidecar.Setup() to fail while running, got nil")
	}

	_ = s.Stop()
	_ = s.Wait()
}

func TestSidecar_PythonPathAndEnv(t *testing.T) {
	tmpDir := t.TempDir()
	modulePath := filepath.Join(tmpDir, "path_check.py")
	moduleContent := `
import sys, os
print("SYSPATH:" + str(sys.path))
print("CUSTOM_VAR:" + os.environ.get("CUSTOM_VAR", ""))
sys.exit(0)
`
	if err := os.WriteFile(modulePath, []byte(moduleContent), 0644); err != nil {
		t.Fatalf("failed to write path_check module: %v", err)
	}

	customPath := filepath.Join(tmpDir, "custom_python_path")
	if err := os.MkdirAll(customPath, 0755); err != nil {
		t.Fatalf("failed to create custom path: %v", err)
	}

	var stdout bytes.Buffer
	cfg := pythonsidecar.Config{
		Module:     "path_check",
		Stdout:     &stdout,
		PythonPath: customPath,
		Env:        []string{"CUSTOM_VAR=test_value_123"},
	}
	t.Setenv("PYTHONPATH", tmpDir)

	s := pythonsidecar.New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	if err := s.Wait(); err != nil {
		t.Fatalf("Wait() failed: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, customPath) {
		t.Errorf("expected sys.path to contain customPath, got output:\n%s", out)
	}
	if !strings.Contains(out, "CUSTOM_VAR:test_value_123") {
		t.Errorf("expected custom env var in output, got output:\n%s", out)
	}
}
