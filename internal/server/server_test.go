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

package server

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/ax/internal/controller"
	"github.com/google/ax/internal/controller/eventlog"
	"github.com/google/ax/internal/controller/eventlog/eventlogtest"
	"github.com/google/ax/internal/harness"
	"github.com/google/ax/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type dummyHarness struct{}

func (d *dummyHarness) Start(ctx context.Context, conversationID string, harnessConfig []byte) (harness.Execution, error) {
	return nil, nil
}

func TestFileService_ReadFile(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.txt")
	expectedContent := []byte("hello from file service")
	if err := os.WriteFile(filePath, expectedContent, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	el := &eventlogtest.MemoryEventLog{}
	eventLogBuilder := func() (eventlog.EventLog, error) {
		return el, nil
	}
	reg := controller.NewRegistry()
	_ = reg.RegisterHarness("default", &dummyHarness{})
	_ = reg.SetDefaultHarness("default")

	c, err := controller.New(context.Background(), controller.Config{
		Registry:        reg,
		EventLogBuilder: eventLogBuilder,
	})
	if err != nil {
		t.Fatalf("failed to create controller: %v", err)
	}

	srv := New(c)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	grpcServer := grpc.NewServer()
	proto.RegisterFileServiceServer(grpcServer, srv)
	go grpcServer.Serve(lis)
	defer grpcServer.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to dial server: %v", err)
	}
	defer conn.Close()

	client := proto.NewFileServiceClient(conn)
	resp, err := client.ReadFile(context.Background(), &proto.ReadFileRequest{
		ConversationId: "conv-123",
		Path:           filePath,
	})
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	if string(resp.GetContent()) != string(expectedContent) {
		t.Errorf("ReadFile content = %q, want %q", string(resp.GetContent()), string(expectedContent))
	}
}
