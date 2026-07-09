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
	"testing/fstest"
	"time"

	"github.com/google/ax/internal/pythonsidecar"
	"github.com/google/ax/python"
)

func TestSetup_EmbeddedFS(t *testing.T) {
	if _, err := fs.Stat(python.FS, "antigravity/__pycache__"); err == nil {
		t.Errorf("expected antigravity/__pycache__ to be ignored when embedding, but it was found")
	}

	testFS := fstest.MapFS{
		"antigravity/harness_server.py": &fstest.MapFile{Data: []byte("print('hello')")},
		"antigravity/requirements.txt":  &fstest.MapFile{Data: []byte("# empty requirements for test\n")},
		"proto/ax_pb2.py":               &fstest.MapFile{Data: []byte("print('proto')")},
	}

	targetDir := filepath.Join(t.TempDir(), "home")
	axDir := filepath.Join(targetDir, ".ax")
	t.Setenv("HOME", targetDir)

	opts := pythonsidecar.SetupOptions{
		FS: testFS,
	}
	gotDir, err := pythonsidecar.Setup(context.Background(), opts)
	if err != nil {
		t.Fatalf("Setup() failed: %v", err)
	}
	if !strings.Contains(gotDir, "python") {
		t.Errorf("expected gotDir to contain extracted python directory in PYTHONPATH, got %q", gotDir)
	}

	// Verify files were extracted under TargetDir/python
	harnessPath := filepath.Join(axDir, "python", "antigravity", "harness_server.py")
	if _, err := os.Stat(harnessPath); err != nil {
		t.Errorf("expected file %s to exist, got stat error: %v", harnessPath, err)
	}
	protoPath := filepath.Join(axDir, "python", "proto", "ax_pb2.py")
	if _, err := os.Stat(protoPath); err != nil {
		t.Errorf("expected file %s to exist, got stat error: %v", protoPath, err)
	}

	// Verify that subsequent Setup calls when TargetDir exists succeed without re-extracting
	if _, err := pythonsidecar.Setup(context.Background(), opts); err != nil {
		t.Fatalf("subsequent Setup() failed when TargetDir exists: %v", err)
	}
}

func TestSidecar_PythonPath(t *testing.T) {
	tmpDir := t.TempDir()
	customPath := filepath.Join(tmpDir, "custom_python_path")
	if err := os.MkdirAll(customPath, 0755); err != nil {
		t.Fatalf("failed to create custom path: %v", err)
	}

	modulePath := filepath.Join(customPath, "path_check.py")
	moduleContent := `
import sys, os
print("SYSPATH:" + str(sys.path))
sys.exit(0)
`
	if err := os.WriteFile(modulePath, []byte(moduleContent), 0644); err != nil {
		t.Fatalf("failed to write path_check module: %v", err)
	}

	var stdout bytes.Buffer
	cfg := pythonsidecar.Config{
		Module: "path_check",
		Stdout: &stdout,
	}

	s := pythonsidecar.New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.Start(ctx, customPath); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	if err := s.Wait(); err != nil {
		t.Fatalf("Wait() failed: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, customPath) {
		t.Errorf("expected sys.path to contain customPath, got output:\n%s", out)
	}
}
