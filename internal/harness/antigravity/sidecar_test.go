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

// The tests below re-exec the test binary with AX_TEST_FAKE_SIDECAR set to
// stand in for the real Python sidecar. TestMain intercepts the env var and
// runs a gRPC health server for the lifetime of the process. The value of the
// env var selects the health registration shape:
//   - "agy":     registers "harness.antigravity" SERVING (behaves like a real sidecar)
//   - "foreign": registers only "" SERVING (mimics an unrelated gRPC service)
func TestMain(m *testing.M) {
	switch os.Getenv("AX_TEST_FAKE_SIDECAR") {
	case "agy":
		runFakeSidecar(true)
		return
	case "foreign":
		runFakeSidecar(false)
		return
	}
	os.Exit(m.Run())
}

// runFakeSidecar binds a gRPC server on --host:--port and blocks. If
// registerAGY is true, it registers the fully-qualified Antigravity health
// service name; otherwise only the overall "" entry (a stand-in for a random
// gRPC service that happens to be on the port).
func runFakeSidecar(registerAGY bool) {
	var host, port string
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
	if registerAGY {
		hs.SetServingStatus(healthServiceName, grpc_health_v1.HealthCheckResponse_SERVING)
	}
	grpc_health_v1.RegisterHealthServer(s, hs)
	if err := s.Serve(lis); err != nil {
		panic("fake sidecar serve: " + err.Error())
	}
}

// TestProbeIdentity_AGYSidecar: a server that registers the Antigravity
// health service name is classified as probeAGYServing.
func TestProbeIdentity_AGYSidecar(t *testing.T) {
	addr := harnesstest.StartHarnessServer(t, &harnesstest.MockHarnessServer{})
	result, err := probeIdentity(context.Background(), addr, 2*time.Second)
	if err != nil {
		t.Fatalf("probeIdentity: %v", err)
	}
	if result != probeAGYServing {
		t.Errorf("result = %s, want probeAGYServing", result)
	}
}

// TestProbeIdentity_ForeignService: a gRPC server that speaks the health
// protocol but does not register the Antigravity service name is classified
// as probeOtherService -- this is the "port is in use by something else" case
// EnsureSidecar surfaces as a mis-config error.
func TestProbeIdentity_ForeignService(t *testing.T) {
	addr := startForeignHealthServer(t)
	result, err := probeIdentity(context.Background(), addr, 2*time.Second)
	if err != nil {
		t.Fatalf("probeIdentity: %v", err)
	}
	if result != probeOtherService {
		t.Errorf("result = %s, want probeOtherService", result)
	}
}

// TestProbeIdentity_NoService: an empty port is classified as probeNoService,
// which authorizes EnsureSidecar to fork on top of it.
func TestProbeIdentity_NoService(t *testing.T) {
	port := freePort(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	result, err := probeIdentity(context.Background(), addr, 2*time.Second)
	if err != nil {
		t.Fatalf("probeIdentity: %v", err)
	}
	if result != probeNoService {
		t.Errorf("result = %s, want probeNoService", result)
	}
}

// TestEnsureSidecar_ReusesExisting: when a real-shaped AGY sidecar is already
// serving at the configured endpoint (e.g. user ran `ax harness` in another
// terminal), EnsureSidecar reuses it rather than forking a duplicate.
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

// TestEnsureSidecar_ErrorOnForeignService: when the configured endpoint is
// occupied by a gRPC service that is not an Antigravity sidecar, EnsureSidecar
// returns an error asking the user to update ax.yaml. It must NOT try to fork
// on top of the foreign service.
func TestEnsureSidecar_ErrorOnForeignService(t *testing.T) {
	addr := startForeignHealthServer(t)
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host/port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}

	_, err = EnsureSidecar(context.Background(), SidecarConfig{Host: host, Port: port})
	if err == nil {
		t.Fatal("expected error when port occupied by foreign service, got nil")
	}
	if !strings.Contains(err.Error(), "does not respond as an Antigravity harness") {
		t.Errorf("expected error to mention foreign service, got: %v", err)
	}
	if !strings.Contains(err.Error(), "ax.yaml") {
		t.Errorf("expected error to point user at ax.yaml, got: %v", err)
	}
}

// TestEnsureSidecar_ForksWhenAbsent: an empty port triggers a fork of the
// configured sidecar command, waits for readiness, and produces a Sidecar
// with Forked=true. Close() tears the child down.
func TestEnsureSidecar_ForksWhenAbsent(t *testing.T) {
	port := freePort(t)

	sc, err := EnsureSidecar(context.Background(), SidecarConfig{
		Host:    "127.0.0.1",
		Port:    port,
		Command: []string{os.Args[0]},
		Env:     append(os.Environ(), "AX_TEST_FAKE_SIDECAR=agy"),
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
	// Reachable and identified as AGY after start.
	result, err := probeIdentity(context.Background(), sc.Addr, 2*time.Second)
	if err != nil || result != probeAGYServing {
		t.Errorf("post-fork probeIdentity = (%s, %v), want (probeAGYServing, nil)", result, err)
	}

	// Close tears down the forked process; the port should free up shortly.
	if err := sc.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		result, _ := probeIdentity(context.Background(), sc.Addr, 200*time.Millisecond)
		if result == probeNoService {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("port %s still reachable after Close (expected probeNoService)", sc.Addr)
}

// TestEnsureSidecar_StartupTimeout: when the forked command runs but never
// serves the Antigravity health service, EnsureSidecar surfaces a
// "did not become healthy" error and reaps the child so the caller doesn't
// leak a process.
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

// startForeignHealthServer starts a gRPC server that implements the health
// protocol but does NOT register the "harness.antigravity" service name. It
// stands in for any other gRPC service that might happen to occupy the port
// the user configured for the AGY sidecar.
func startForeignHealthServer(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := grpc.NewServer()
	hs := health.NewServer()
	// Register only the overall "" entry -- no service-specific entries.
	// gRPC health protocol returns NOT_FOUND for unknown service names.
	hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(s, hs)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
	return lis.Addr().String()
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
