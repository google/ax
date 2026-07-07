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

package cliutil

import (
	"context"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/google/ax/internal/config"
	"github.com/google/ax/internal/harness/harnesstest"
)

func TestNewControllerFromConfig_DefaultHarness(t *testing.T) {
	// Sidecar autostart would try to fork python3 here; the harness client
	// itself never dials until a request is made, so the test only needs to
	// build the controller.
	t.Setenv("AX_ANTIGRAVITY_NO_AUTOSTART", "1")

	cfg := &config.Config{
		EventLog: config.EventLogConfig{
			SQLiteConfig: config.SQLiteConfig{
				Filename: filepath.Join(t.TempDir(), "log.sqlite"),
			},
		},
		Harnesses: config.HarnessesConfig{
			Antigravity: config.AntigravityHarnessConfig{
				Default:  true,
				Endpoint: "localhost:50053",
			},
		},
	}

	c, err := NewControllerFromConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewControllerFromConfig: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil controller")
	}
	c.Close()
}

func TestNewControllerFromConfig_BuiltinSubstrate(t *testing.T) {
	t.Setenv("AX_SUBSTRATE", "1")

	cfg := &config.Config{
		EventLog: config.EventLogConfig{
			SQLiteConfig: config.SQLiteConfig{
				Filename: filepath.Join(t.TempDir(), "log.sqlite"),
			},
		},
		Harnesses: config.HarnessesConfig{
			Antigravity: config.AntigravityHarnessConfig{
				Default: true,
			},
		},
	}

	c, err := NewControllerFromConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewControllerFromConfig: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil controller")
	}
	c.Close()
}

func TestNewControllerFromConfig_CustomHarnessRequiresSubstrateMode(t *testing.T) {
	t.Setenv("AX_SUBSTRATE", "")
	t.Setenv("AX_ANTIGRAVITY_NO_AUTOSTART", "1")

	cfg := &config.Config{
		EventLog: config.EventLogConfig{
			SQLiteConfig: config.SQLiteConfig{
				Filename: filepath.Join(t.TempDir(), "log.sqlite"),
			},
		},
		Harnesses: config.HarnessesConfig{
			Substrate: []config.SubstrateHarnessConfig{
				{ID: "custom", Namespace: "team-ns", Template: "custom-template"},
			},
		},
	}

	_, err := NewControllerFromConfig(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for custom substrate harness without AX_SUBSTRATE=1, got nil")
	}
	if !strings.Contains(err.Error(), "AX_SUBSTRATE=1") {
		t.Errorf("expected error to mention AX_SUBSTRATE=1, got: %v", err)
	}
}

func TestNewControllerFromConfig_CustomHarnessInSubstrateMode(t *testing.T) {
	t.Setenv("AX_SUBSTRATE", "1")

	cfg := &config.Config{
		EventLog: config.EventLogConfig{
			SQLiteConfig: config.SQLiteConfig{
				Filename: filepath.Join(t.TempDir(), "log.sqlite"),
			},
		},
		Harnesses: config.HarnessesConfig{
			Substrate: []config.SubstrateHarnessConfig{
				{ID: "custom", Namespace: "team-ns", Template: "custom-template"},
			},
		},
	}

	c, err := NewControllerFromConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewControllerFromConfig: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil controller")
	}
	c.Close()
}

// TestNewControllerFromConfig_AutostartReusesExistingSidecar covers CUJ-B end
// to end through the cliutil layer: when the endpoint already hosts an
// Antigravity harness (registered "harness.antigravity" as SERVING), the
// controller is built successfully without forking or erroring.
func TestNewControllerFromConfig_AutostartReusesExistingSidecar(t *testing.T) {
	// StartHarnessServer registers both "" and "harness.antigravity" as
	// SERVING, so this stands in for a real AGY sidecar already running at
	// the configured endpoint.
	addr := harnesstest.StartHarnessServer(t, &harnesstest.MockHarnessServer{})

	cfg := &config.Config{
		EventLog: config.EventLogConfig{
			SQLiteConfig: config.SQLiteConfig{
				Filename: filepath.Join(t.TempDir(), "log.sqlite"),
			},
		},
		Harnesses: config.HarnessesConfig{
			Antigravity: config.AntigravityHarnessConfig{
				Default:  true,
				Endpoint: addr,
			},
		},
	}

	c, err := NewControllerFromConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewControllerFromConfig: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil controller")
	}
	c.Close()
}

// TestNewControllerFromConfig_AutostartRejectsForeignService covers CUJ-error:
// when the endpoint hosts a gRPC service that does NOT register the
// "harness.antigravity" health entry, NewControllerFromConfig must fail with a
// clear error pointing the user at ax.yaml rather than silently misusing the
// port.
func TestNewControllerFromConfig_AutostartRejectsForeignService(t *testing.T) {
	addr := startForeignGRPCServer(t)

	cfg := &config.Config{
		EventLog: config.EventLogConfig{
			SQLiteConfig: config.SQLiteConfig{
				Filename: filepath.Join(t.TempDir(), "log.sqlite"),
			},
		},
		Harnesses: config.HarnessesConfig{
			Antigravity: config.AntigravityHarnessConfig{
				Default:  true,
				Endpoint: addr,
			},
		},
	}

	_, err := NewControllerFromConfig(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error when endpoint hosts a foreign service, got nil")
	}
	if !strings.Contains(err.Error(), "does not respond as an Antigravity harness") {
		t.Errorf("expected error to mention foreign service, got: %v", err)
	}
	if !strings.Contains(err.Error(), "ax.yaml") {
		t.Errorf("expected error to point user at ax.yaml, got: %v", err)
	}
}

// TestNewControllerFromConfig_AutostartSkippedForNonLoopback covers the
// isLocalHost short-circuit: when the endpoint is not this machine (e.g. the
// user is pointing at a remote sidecar they manage themselves), autostart is
// skipped entirely and the controller builds without any probe. Without the
// skip, autoStartAntigravitySidecar would probe example.invalid:50053 which
// would take multiple seconds to time out and then return a probe error.
func TestNewControllerFromConfig_AutostartSkippedForNonLoopback(t *testing.T) {
	cfg := &config.Config{
		EventLog: config.EventLogConfig{
			SQLiteConfig: config.SQLiteConfig{
				Filename: filepath.Join(t.TempDir(), "log.sqlite"),
			},
		},
		Harnesses: config.HarnessesConfig{
			Antigravity: config.AntigravityHarnessConfig{
				Default:  true,
				Endpoint: "sidecar.example.invalid:50053",
			},
		},
	}

	c, err := NewControllerFromConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewControllerFromConfig: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil controller")
	}
	c.Close()
}

// TestNewControllerFromConfig_AutostartHonorsNoAutostartEnv verifies that
// AX_ANTIGRAVITY_NO_AUTOSTART=1 disables autostart even when the endpoint is
// loopback and unoccupied. The controller is built without probing or forking;
// the sidecar is the user's responsibility from that point on.
func TestNewControllerFromConfig_AutostartHonorsNoAutostartEnv(t *testing.T) {
	t.Setenv("AX_ANTIGRAVITY_NO_AUTOSTART", "1")

	// Pick a free port and don't listen on it, so absence of a probe is the
	// only reason the test doesn't hang.
	port := freeTCPPort(t)

	cfg := &config.Config{
		EventLog: config.EventLogConfig{
			SQLiteConfig: config.SQLiteConfig{
				Filename: filepath.Join(t.TempDir(), "log.sqlite"),
			},
		},
		Harnesses: config.HarnessesConfig{
			Antigravity: config.AntigravityHarnessConfig{
				Default:  true,
				Endpoint: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
			},
		},
	}

	c, err := NewControllerFromConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewControllerFromConfig: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil controller")
	}
	c.Close()
}

// startForeignGRPCServer starts a gRPC server that implements the health
// protocol but does NOT register the "harness.antigravity" service name. It
// stands in for an unrelated gRPC service happening to occupy the port a user
// mistakenly configured for the AGY sidecar.
func startForeignGRPCServer(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := grpc.NewServer()
	hs := health.NewServer()
	// Only "" registered -- no service-specific entries. gRPC health protocol
	// returns NOT_FOUND for unknown service names like "harness.antigravity".
	hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(s, hs)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
	return lis.Addr().String()
}

// freeTCPPort returns a free TCP port on 127.0.0.1. The port is released
// before returning; the caller may race with another process but that is
// acceptable for tests that bind immediately.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
