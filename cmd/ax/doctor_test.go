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
	"bytes"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDoctorCommandRegistration(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"doctor"})
	if err != nil {
		t.Fatalf("failed to find 'doctor' subcommand: %v", err)
	}
	if cmd.Name() != "doctor" {
		t.Fatalf("expected subcommand name 'doctor', got '%s'", cmd.Name())
	}

	if endpointFlag := cmd.Flags().Lookup("endpoint"); endpointFlag == nil {
		t.Errorf("expected --endpoint flag on doctor command")
	}
}

func TestSampler_NoSidecar(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("AX_DURABLE_DIR", tmpDir)

	sampler := NewSampler("127.0.0.1:0")
	stats := sampler.Sample()

	if stats.SidecarAlive {
		t.Errorf("expected SidecarAlive to be false, got true")
	}
	if stats.SidecarPID != 0 {
		t.Errorf("expected SidecarPID to be 0, got %d", stats.SidecarPID)
	}
	if stats.AssetsDir != filepath.Join(tmpDir, ".ax") {
		t.Errorf("expected AssetsDir %s, got %s", filepath.Join(tmpDir, ".ax"), stats.AssetsDir)
	}
}

func TestSampler_WithActiveProcessAndEndpoint(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("AX_DURABLE_DIR", tmpDir)

	cmd := exec.Command("sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start sleep process: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	axDir := filepath.Join(tmpDir, ".ax")
	if err := os.MkdirAll(axDir, 0755); err != nil {
		t.Fatalf("failed to create axDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(axDir, "sidecar.pid"), []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0644); err != nil {
		t.Fatalf("failed to write pid file: %v", err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer l.Close()

	sampler := NewSampler(l.Addr().String())
	stats := sampler.Sample()

	if !stats.SidecarAlive {
		t.Errorf("expected SidecarAlive to be true")
	}
	if stats.SidecarPID != cmd.Process.Pid {
		t.Errorf("expected SidecarPID %d, got %d", cmd.Process.Pid, stats.SidecarPID)
	}
	if !stats.EndpointActive {
		t.Errorf("expected EndpointActive to be true")
	}
}

func TestDisplay_Render(t *testing.T) {
	var buf bytes.Buffer
	display := NewDisplay(&buf)

	stats := Stats{
		AssetsDir:     "/test/assets",
		SidecarPID:    1234,
		SidecarAlive:  true,
		SidecarCPU:    "0.5%",
		SidecarRSS:    "45.2 MB",
		SidecarUptime: "02:15",
		SidecarCmd:    "python3 -m test",
		Endpoint:      "127.0.0.1:50053",
		PythonVersion: "Python 3.13.0",
		GoVersion:     "go1.26.3",
		NumGoroutine:  5,
		MemAllocMB:    12.4,
	}

	display.Render(stats)
	output := buf.String()

	if !strings.Contains(output, "ax doctor") {
		t.Errorf("expected output to contain 'ax doctor'")
	}
	if !strings.Contains(output, "1234") {
		t.Errorf("expected output to contain PID '1234'")
	}
	if !strings.Contains(output, "RUNNING") {
		t.Errorf("expected output to contain 'RUNNING'")
	}
	if !strings.Contains(output, "Assets:") {
		t.Errorf("expected output to contain 'Assets:'")
	}
	if !strings.Contains(output, "Environment:") {
		t.Errorf("expected output to contain 'Environment:'")
	}
}

func TestPrintStatus(t *testing.T) {
	var buf bytes.Buffer
	printStatus(&buf, "127.0.0.1:0")

	if buf.Len() == 0 {
		t.Errorf("expected non-empty printStatus output")
	}
}
