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
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/google/ax/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// Mock servers for testing interceptors
type mockConversationServer struct {
	proto.UnimplementedConversationServiceServer
}

func (m *mockConversationServer) DeleteConversation(ctx context.Context, req *proto.DeleteConversationRequest) (*proto.DeleteConversationResponse, error) {
	if req.ConversationId == "fail" {
		return nil, status.Error(codes.InvalidArgument, "mock error")
	}
	return &proto.DeleteConversationResponse{}, nil
}

type mockExecutionServer struct {
	proto.UnimplementedExecutionServiceServer
}

func (m *mockExecutionServer) Exec(req *proto.ExecRequest, stream proto.ExecutionService_ExecServer) error {
	if req.ConversationId == "fail" {
		return status.Error(codes.InvalidArgument, "mock error")
	}
	return stream.Send(&proto.ExecResponse{})
}

const bufSize = 1024 * 1024

func setupTestServer(t *testing.T) (*grpc.ClientConn, func()) {
	lis := bufconn.Listen(bufSize)
	s := grpc.NewServer(
		grpc.ChainUnaryInterceptor(LoggingInterceptor),
		grpc.ChainStreamInterceptor(StreamLoggingInterceptor),
	)

	proto.RegisterConversationServiceServer(s, &mockConversationServer{})
	proto.RegisterExecutionServiceServer(s, &mockExecutionServer{})

	go func() {
		if err := s.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			t.Errorf("Server exited with error: %v", err)
		}
	}()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("Failed to dial bufnet: %v", err)
	}

	cleanup := func() {
		conn.Close()
		s.GracefulStop()
		lis.Close()
	}

	return conn, cleanup
}

type logEntry struct {
	Time           string `json:"time"`
	Level          string `json:"level"`
	Msg            string `json:"msg"`
	Method         string `json:"method"`
	ConversationID string `json:"conversation_id,omitempty"`
	Duration       int64  `json:"duration,omitempty"` // in nanoseconds
	Error          string `json:"error,omitempty"`
}

func TestLoggingInterceptors(t *testing.T) {
	conn, cleanup := setupTestServer(t)
	defer cleanup()

	// Capture slog output to a buffer
	var logBuf bytes.Buffer
	testLogger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	oldLogger := slog.Default()
	slog.SetDefault(testLogger)
	defer slog.SetDefault(oldLogger)

	convClient := proto.NewConversationServiceClient(conn)
	executionClient := proto.NewExecutionServiceClient(conn)

	t.Run("Unary Success", func(t *testing.T) {
		logBuf.Reset()
		ctx := context.Background()
		_, err := convClient.DeleteConversation(ctx, &proto.DeleteConversationRequest{ConversationId: "conv-123"})
		if err != nil {
			t.Fatalf("DeleteConversation failed: %v", err)
		}

		entries := parseLogs(t, &logBuf)
		if len(entries) != 2 {
			t.Fatalf("Expected 2 log entries, got %d", len(entries))
		}

		// Verify Start Log
		if entries[0].Msg != "Handling unary request" {
			t.Errorf("Expected start log msg 'Handling unary request', got %q", entries[0].Msg)
		}
		if entries[0].Method != "/ax.ConversationService/DeleteConversation" {
			t.Errorf("Expected method '/ax.ConversationService/DeleteConversation', got %q", entries[0].Method)
		}
		if entries[0].ConversationID != "conv-123" {
			t.Errorf("Expected conversation_id 'conv-123', got %q", entries[0].ConversationID)
		}

		// Verify End Log
		if entries[1].Msg != "Request completed" {
			t.Errorf("Expected end log msg 'Request completed', got %q", entries[1].Msg)
		}
		if entries[1].Level != "INFO" {
			t.Errorf("Expected Level INFO, got %s", entries[1].Level)
		}
	})

	t.Run("Unary Failure", func(t *testing.T) {
		logBuf.Reset()
		ctx := context.Background()
		_, err := convClient.DeleteConversation(ctx, &proto.DeleteConversationRequest{ConversationId: "fail"})
		if err == nil {
			t.Fatal("Expected DeleteConversation to fail")
		}

		entries := parseLogs(t, &logBuf)
		if len(entries) != 2 {
			t.Fatalf("Expected 2 log entries, got %d", len(entries))
		}

		// Verify End Log
		if entries[1].Msg != "Request failed" {
			t.Errorf("Expected end log msg 'Request failed', got %q", entries[1].Msg)
		}
		if entries[1].Level != "ERROR" {
			t.Errorf("Expected Level ERROR, got %s", entries[1].Level)
		}
		if !strings.Contains(entries[1].Error, "mock error") {
			t.Errorf("Expected error details to contain 'mock error', got %q", entries[1].Error)
		}
	})

	t.Run("Stream Success", func(t *testing.T) {
		logBuf.Reset()
		ctx := context.Background()
		stream, err := executionClient.Exec(ctx, &proto.ExecRequest{ConversationId: "conv-456"})
		if err != nil {
			t.Fatalf("Exec stream init failed: %v", err)
		}

		// Consume stream
		for {
			_, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("Error receiving from stream: %v", err)
			}
		}

		entries := parseLogs(t, &logBuf)
		if len(entries) != 2 {
			t.Fatalf("Expected 2 log entries, got %d", len(entries))
		}

		// Verify Start Log
		if entries[0].Msg != "Handling stream request" {
			t.Errorf("Expected start log msg 'Handling stream request', got %q", entries[0].Msg)
		}
		if entries[0].Method != "/ax.ExecutionService/Exec" {
			t.Errorf("Expected method '/ax.ExecutionService/Exec', got %q", entries[0].Method)
		}

		// Verify End Log
		if entries[1].Msg != "Stream completed" {
			t.Errorf("Expected end log msg 'Stream completed', got %q", entries[1].Msg)
		}
		if entries[1].Level != "INFO" {
			t.Errorf("Expected Level INFO, got %s", entries[1].Level)
		}
	})

	t.Run("Stream Failure", func(t *testing.T) {
		logBuf.Reset()
		ctx := context.Background()
		stream, err := executionClient.Exec(ctx, &proto.ExecRequest{ConversationId: "fail"})
		if err != nil {
			t.Fatalf("Exec stream init failed: %v", err)
		}

		// Consume stream (should fail)
		_, err = stream.Recv()
		if err == nil || err == io.EOF {
			t.Fatal("Expected stream read to fail")
		}

		entries := parseLogs(t, &logBuf)
		if len(entries) != 2 {
			t.Fatalf("Expected 2 log entries, got %d", len(entries))
		}

		// Verify End Log
		if entries[1].Msg != "Stream failed" {
			t.Errorf("Expected end log msg 'Stream failed', got %q", entries[1].Msg)
		}
		if entries[1].Level != "ERROR" {
			t.Errorf("Expected Level ERROR, got %s", entries[1].Level)
		}
		if !strings.Contains(entries[1].Error, "mock error") {
			t.Errorf("Expected error details to contain 'mock error', got %q", entries[1].Error)
		}
	})
}

func parseLogs(t *testing.T, buf *bytes.Buffer) []logEntry {
	t.Helper()
	var entries []logEntry
	decoder := json.NewDecoder(buf)
	for {
		var entry logEntry
		if err := decoder.Decode(&entry); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("Failed to decode log JSON: %v. Raw buffer: %s", err, buf.String())
		}
		entries = append(entries, entry)
	}
	return entries
}
