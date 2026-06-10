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

package harness

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
)

// startHealthTestServer starts a gRPC server on a random local port. If hs is
// non-nil the standard health service is registered. Returns the listen address.
func startHealthTestServer(t *testing.T, hs *health.Server) string {
	t.Helper()
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	s := grpc.NewServer()
	if hs != nil {
		grpc_health_v1.RegisterHealthServer(s, hs)
	}
	go func() {
		_ = s.Serve(lis)
	}()
	t.Cleanup(s.Stop)
	return lis.Addr().String()
}

func dialTestConn(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to dial %s: %v", addr, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestWaitForHealthy_Serving(t *testing.T) {
	hs := health.NewServer()
	hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	conn := dialTestConn(t, startHealthTestServer(t, hs))

	if err := waitForHealthy(context.Background(), conn, 5*time.Second); err != nil {
		t.Fatalf("expected healthy, got %v", err)
	}
}

func TestWaitForHealthy_UnimplementedProceeds(t *testing.T) {
	// Server is up but does not register the health service.
	conn := dialTestConn(t, startHealthTestServer(t, nil))

	if err := waitForHealthy(context.Background(), conn, 5*time.Second); err != nil {
		t.Fatalf("expected to proceed when health is unimplemented, got %v", err)
	}
}

func TestWaitForHealthy_TimesOut(t *testing.T) {
	hs := health.NewServer()
	hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	conn := dialTestConn(t, startHealthTestServer(t, hs))

	if err := waitForHealthy(context.Background(), conn, 500*time.Millisecond); err == nil {
		t.Fatal("expected timeout error while NOT_SERVING, got nil")
	}
}

func TestWaitForHealthy_StatusChange(t *testing.T) {
	hs := health.NewServer()
	hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	conn := dialTestConn(t, startHealthTestServer(t, hs))

	go func() {
		time.Sleep(150 * time.Millisecond)
		hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	}()

	if err := waitForHealthy(context.Background(), conn, 5*time.Second); err != nil {
		t.Fatalf("expected healthy after status flip, got %v", err)
	}
}

func TestWaitForHealthy_ServerDown(t *testing.T) {
	// Reserve a port then release it so nothing is listening there.
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	addr := lis.Addr().String()
	lis.Close()
	conn := dialTestConn(t, addr)

	if err := waitForHealthy(context.Background(), conn, 500*time.Millisecond); err == nil {
		t.Fatal("expected timeout error when server is down, got nil")
	}
}
