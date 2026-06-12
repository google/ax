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
	"log/slog"
	"time"

	"google.golang.org/grpc"
)

type conversationer interface {
	GetConversationId() string
}

// LoggingInterceptor logs unary RPC details, including latency, errors, and conversation ID if present.
func LoggingInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	start := time.Now()
	logger := slog.With(slog.String("method", info.FullMethod))

	// Extract conversation_id if the request supports it
	if conv, ok := req.(conversationer); ok {
		if id := conv.GetConversationId(); id != "" {
			logger = logger.With(slog.String("conversation_id", id))
		}
	}

	logger.InfoContext(ctx, "Handling unary request")
	resp, err := handler(ctx, req)

	duration := time.Since(start)
	if err != nil {
		logger.ErrorContext(ctx, "Request failed",
			slog.Duration("duration", duration),
			slog.Any("error", err),
		)
	} else {
		logger.InfoContext(ctx, "Request completed",
			slog.Duration("duration", duration),
		)
	}
	return resp, err
}

// StreamLoggingInterceptor logs stream RPC connection details and latency.
func StreamLoggingInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	start := time.Now()
	ctx := ss.Context()
	logger := slog.With(slog.String("method", info.FullMethod))

	logger.InfoContext(ctx, "Handling stream request")
	err := handler(srv, ss)

	duration := time.Since(start)
	if err != nil {
		logger.ErrorContext(ctx, "Stream failed",
			slog.Duration("duration", duration),
			slog.Any("error", err),
		)
	} else {
		logger.InfoContext(ctx, "Stream completed",
			slog.Duration("duration", duration),
		)
	}
	return err
}
