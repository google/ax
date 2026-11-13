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

package server_test

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/google/ax/internal/server"
	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/internal/substrate/substratetest"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// TestHandlerTracesRPCs checks that RPCs served through Handler, the h2c path
// ax-server uses, produce server spans that join the caller's trace.
func TestHandlerTracesRPCs(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.NeverSample())), sdktrace.WithSpanProcessor(spans))
	otel.SetTracerProvider(tp)
	// Restoring the old global value would keep its delegate bound to tp, so
	// install a no-op provider instead.
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })

	srv := server.NewServer(memory.NewStore(), newTestReconciler(substratetest.NewServer(t)))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: srv.Handler()}
	httpServer.Protocols = new(http.Protocols)
	httpServer.Protocols.SetHTTP1(true)
	httpServer.Protocols.SetUnencryptedHTTP2(true)
	go func() { _ = httpServer.Serve(ln) }()
	t.Cleanup(func() { _ = httpServer.Close() })

	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := v1alpha1.NewAXClient(conn)

	const (
		traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
		spanID  = "00f067aa0ba902b7"
	)
	// A sampled caller is traced even though new traces are never sampled
	// here; an unsampled one is not.
	for _, flags := range []string{"01", "00"} {
		ctx := metadata.AppendToOutgoingContext(context.Background(), "traceparent", "00-"+traceID+"-"+spanID+"-"+flags)
		if _, err := client.ListTasks(ctx, &v1alpha1.ListTasksRequest{Atespace: "default"}); err != nil {
			t.Fatal(err)
		}
	}

	// gRPC ends the server span after it writes the status, so the span can
	// end after the client call returns. An unsampled span never reaches the
	// recorder, so once the sampled span is there, the count is final.
	deadline := time.Now().Add(5 * time.Second)
	for len(spans.Ended()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	ended := spans.Ended()
	if len(ended) != 1 {
		t.Fatalf("got %d spans, want one for the sampled caller only", len(ended))
	}
	s := ended[0]
	if s.Name() != "ax.v1alpha1.AX/ListTasks" || s.SpanKind() != trace.SpanKindServer {
		t.Errorf("span = %q (%v), want the ListTasks server span", s.Name(), s.SpanKind())
	}
	if s.SpanContext().TraceID().String() != traceID || s.Parent().SpanID().String() != spanID {
		t.Errorf("span trace %s parent %s, want trace %s parent %s", s.SpanContext().TraceID(), s.Parent().SpanID(), traceID, spanID)
	}
}
