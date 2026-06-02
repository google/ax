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

package controller2

// TODO(lhuan): Setup a better automated testing framework

import (
	"context"
	"fmt"
	"net"
	"testing"

	"google.golang.org/grpc"

	"github.com/google/ax/internal/config"
	"github.com/google/ax/proto"
)

type mockAgentServer struct {
	proto.UnimplementedAgentServiceServer
	healthy bool
}

func startMockGRPCServer(t *testing.T, healthy bool) (string, func()) {
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	s := grpc.NewServer()
	proto.RegisterAgentServiceServer(s, &mockAgentServer{healthy: healthy})
	go func() {
		if err := s.Serve(lis); err != nil {
			// server might be closed
		}
	}()
	return lis.Addr().String(), func() {
		s.Stop()
		lis.Close()
	}
}

func TestRegistry_GracefulShutdown(t *testing.T) {
	r := NewRegistry()

	address, cleanup := startMockGRPCServer(t, true)
	defer cleanup()

	// Register multiple agents to create workload
	for i := range 50 {
		err := r.RegisterRemote(context.Background(), config.RemoteAgentConfig{
			ID:      fmt.Sprintf("remote-shutdown-test-%d", i),
			Name:    "Shutdown Test Remote",
			Address: address,
		})
		if err != nil {
			t.Fatalf("Failed to register remote agent for shutdown test: %v", err)
		}
	}

	// Close should return specific errors for failed agents, but NOT panic or deadlock
	// We are testing for absence of panic/deadlock here.
	_ = r.Close()
}

func TestRegistry_LegacyAgentValidation(t *testing.T) {
	r := NewRegistry()

	err := r.RegisterRemote(context.Background(), config.RemoteAgentConfig{
		ID:      "legacy-remote-agent",
		Name:    "Legacy Agent",
		Address: "localhost:8494",
	})
	if err != nil {
		t.Fatalf("failed to register remote agent: %v", err)
	}

	_, err = r.Harness("legacy-remote-agent")
	if err == nil {
		t.Fatal("expected error for legacy agent lookup in V2 mode, got nil")
	}

	expectedErr := `agent "legacy-remote-agent" is registered under legacy agent config but has no V2 harness interface implementation`
	if err.Error() != expectedErr {
		t.Errorf("expected error message %q, got %q", expectedErr, err.Error())
	}
}
