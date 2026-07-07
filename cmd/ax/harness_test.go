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
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func TestSetHarnessWorkDir(t *testing.T) {
	// os.Chdir is global process state; save and restore it around the test.
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	t.Run("unset leaves the working directory unchanged", func(t *testing.T) {
		t.Setenv("AX_HARNESS_WORKDIR", "")
		before, _ := os.Getwd()
		if err := setHarnessWorkDir(); err != nil {
			t.Fatalf("setHarnessWorkDir: %v", err)
		}
		after, _ := os.Getwd()
		if before != after {
			t.Errorf("working directory changed: %q -> %q", before, after)
		}
	})

	t.Run("set changes the working directory", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("AX_HARNESS_WORKDIR", dir)
		if err := setHarnessWorkDir(); err != nil {
			t.Fatalf("setHarnessWorkDir: %v", err)
		}
		got, err := os.Getwd()
		if err != nil {
			t.Fatalf("getwd: %v", err)
		}
		want, _ := filepath.EvalSymlinks(dir)
		gotResolved, _ := filepath.EvalSymlinks(got)
		if gotResolved != want {
			t.Errorf("working directory = %q, want %q", gotResolved, want)
		}
	})

	t.Run("missing directory returns an error", func(t *testing.T) {
		t.Setenv("AX_HARNESS_WORKDIR", filepath.Join(t.TempDir(), "does-not-exist"))
		if err := setHarnessWorkDir(); err == nil {
			t.Error("expected an error for a missing directory, got nil")
		}
	})
}

func TestServeReadyz(t *testing.T) {
	// Start a real gRPC server exposing the standard health service; this stands
	// in for the Antigravity Python harness that serveReadyz probes.
	grpcListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for gRPC: %v", err)
	}
	healthServer := health.NewServer()
	grpcServer := grpc.NewServer()
	healthpb.RegisterHealthServer(grpcServer, healthServer)
	go func() { _ = grpcServer.Serve(grpcListener) }()
	t.Cleanup(grpcServer.Stop)
	grpcPort := grpcListener.Addr().(*net.TCPAddr).Port

	// Reserve an ephemeral port for /readyz, then hand it to serveReadyz to bind.
	readyzListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for readyz: %v", err)
	}
	readyzPort := readyzListener.Addr().(*net.TCPAddr).Port
	_ = readyzListener.Close()

	// Start with the harness not serving, then run serveReadyz against it.
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	go serveReadyz(readyzPort, grpcPort)
	url := fmt.Sprintf("http://127.0.0.1:%d/readyz", readyzPort)

	// /readyz re-derives readiness from gRPC health on every request: 503 until
	// the harness is SERVING, 200 once it is, and back to 503 if it stops serving.
	waitReadyz(t, url, http.StatusServiceUnavailable)
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	waitReadyz(t, url, http.StatusOK)
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	waitReadyz(t, url, http.StatusServiceUnavailable)
}

func waitReadyz(t *testing.T, url string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	got := 0
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			got = resp.StatusCode
			_ = resp.Body.Close()
			if got == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("GET %s = %d, want %d", url, got, want)
}
