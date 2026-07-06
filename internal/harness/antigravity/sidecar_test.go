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
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/google/ax/internal/harness/harnesstest"
)

// The tests below re-exec the test binary with AX_TEST_FAKE_SIDECAR=1 to
// stand in for the Python sidecar. TestMain intercepts that env var and runs
// a minimal gRPC health server for the lifetime of the process.
func TestMain(m *testing.M) {
	if os.Getenv("AX_TEST_FAKE_SIDECAR") == "1" {
		runFakeSidecar()
		return
	}
	os.Exit(m.Run())
}

// runFakeSidecar binds a gRPC server that reports SERVING on the address the
// parent test passes via --host/--port and blocks. Any parse or bind error is
// fatal (the parent test will see a non-healthy sidecar and fail with a clear
// message).
func runFakeSidecar() {
	// Args are: [binPath, ..., --host, H, --port, P] as appended by SidecarCommand.
	var host string
	var port string
	args := os.Args
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--host":
			host = args[i+1]
		case "--port":
			port = args[i+1]
		}
	}
	lis, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		panic("fake sidecar listen: " + err.Error())
	}
	s := grpc.NewServer()
	hs := health.NewServer()
	hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(s, hs)
	if err := s.Serve(lis); err != nil {
		panic("fake sidecar serve: " + err.Error())
	}
}

// TestEnsureSidecar_ReusesExisting verifies that when an Antigravity harness is
// already serving at the configured address (e.g. because the user ran
// ax harness in another terminal, or a previous ax exec left one running),
// EnsureSidecar attaches to it instead of forking a duplicate Python process.
func TestEnsureSidecar_ReusesExisting(t *testing.T) {
	addr := harnesstest.StartHarnessServer(t, &harnesstest.MockHarnessServer{})
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host/port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}

	sc, err := EnsureSidecar(context.Background(), SidecarConfig{Host: host, Port: port})
	if err != nil {
		t.Fatalf("EnsureSidecar: %v", err)
	}
	defer sc.Close()

	if sc.Forked {
		t.Errorf("expected reused sidecar (Forked=false), got Forked=true")
	}
	if sc.Addr != addr {
		t.Errorf("Addr = %q, want %q", sc.Addr, addr)
	}
}

// TestEnsureSidecar_ForksWhenAbsent verifies that when nothing is serving at
// the configured address, EnsureSidecar forks the configured command and waits
// for it to become healthy.
func TestEnsureSidecar_ForksWhenAbsent(t *testing.T) {
	port := freePort(t)

	sc, err := EnsureSidecar(context.Background(), SidecarConfig{
		Host:    "127.0.0.1",
		Port:    port,
		Command: []string{os.Args[0]},
		Env:     append(os.Environ(), "AX_TEST_FAKE_SIDECAR=1"),
	})
	if err != nil {
		t.Fatalf("EnsureSidecar: %v", err)
	}
	defer sc.Close()

	if !sc.Forked {
		t.Errorf("expected Forked=true, got Forked=false")
	}
	wantAddr := "127.0.0.1:" + strconv.Itoa(port)
	if sc.Addr != wantAddr {
		t.Errorf("Addr = %q, want %q", sc.Addr, wantAddr)
	}
	if !reachable(context.Background(), sc.Addr, 2*time.Second) {
		t.Errorf("sidecar not reachable at %s after EnsureSidecar returned", sc.Addr)
	}

	// Close tears down the forked process; the port should free up shortly.
	if err := sc.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !reachable(context.Background(), sc.Addr, 200*time.Millisecond) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("sidecar still reachable at %s after Close", sc.Addr)
}

// TestEnsureSidecar_StartupTimeout verifies that when the forked process runs
// but never serves health, EnsureSidecar returns an error rather than blocking
// indefinitely, and reaps the child so the caller doesn't leak a process.
func TestEnsureSidecar_StartupTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sleep(1) not portable to Windows")
	}
	port := freePort(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := EnsureSidecar(ctx, SidecarConfig{
		Host:    "127.0.0.1",
		Port:    port,
		Command: []string{"sleep", "60"},
	})
	if err == nil {
		t.Fatal("expected error when sidecar never becomes healthy, got nil")
	}
	if !strings.Contains(err.Error(), "did not become healthy") {
		t.Errorf("expected 'did not become healthy' in error, got: %v", err)
	}
}

// TestSidecarCommand_DefaultCommand verifies that SidecarCommand falls back to
// the built-in Python argv when Command is left empty, and appends --host/--port.
func TestSidecarCommand_DefaultCommand(t *testing.T) {
	cmd := SidecarCommand(SidecarConfig{Host: "0.0.0.0", Port: 60001})
	want := []string{"python3", "-m", "python.antigravity.harness_server", "--host", "0.0.0.0", "--port", "60001"}
	if !stringSlicesEqual(cmd.Args, want) {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
}

// TestSidecarCommand_CustomCommand verifies that a caller-supplied Command
// replaces the default argv and still has --host/--port appended.
func TestSidecarCommand_CustomCommand(t *testing.T) {
	cmd := SidecarCommand(SidecarConfig{
		Host:    "127.0.0.1",
		Port:    9,
		Command: []string{"my-sidecar", "--flag=x"},
	})
	want := []string{"my-sidecar", "--flag=x", "--host", "127.0.0.1", "--port", "9"}
	if !stringSlicesEqual(cmd.Args, want) {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
}

// freePort returns a currently-unused TCP port on 127.0.0.1. The port is
// released before returning; a caller that binds it may race with another
// process, but this is fine for tests that then start listening on the port
// almost immediately.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
