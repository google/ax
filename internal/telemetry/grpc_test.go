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

package telemetry

import (
	"context"
	"net"
	"slices"
	"testing"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// rpcSeen is what the AX handler saw in its context.
type rpcSeen struct {
	span    trace.SpanContext
	baggage baggage.Baggage
}

type recordingAX struct {
	v1alpha1.UnimplementedAXServer
	seen chan rpcSeen
}

func (s *recordingAX) ListTasks(ctx context.Context, _ *v1alpha1.ListTasksRequest) (*v1alpha1.ListTasksResponse, error) {
	s.seen <- rpcSeen{span: trace.SpanContextFromContext(ctx), baggage: baggage.FromContext(ctx)}
	return &v1alpha1.ListTasksResponse{}, nil
}

// useTestProviders installs global providers that record every span and keep
// metrics for collection, with the RPC duration views.
func useTestProviders(t *testing.T) (*tracetest.SpanRecorder, *sdkmetric.ManualReader) {
	t.Helper()
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithView(rpcDurationViews()...))
	prevTP, prevMP := otel.GetTracerProvider(), otel.GetMeterProvider()
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetMeterProvider(prevMP)
		_ = tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	})
	return spans, reader
}

// startAX serves the AX API with ServerStatsHandler and returns its address.
func startAX(t *testing.T) (string, <-chan rpcSeen) {
	t.Helper()
	seen := make(chan rpcSeen, 4)
	srv := grpc.NewServer(grpc.StatsHandler(ServerStatsHandler(&v1alpha1.AX_ServiceDesc)))
	v1alpha1.RegisterAXServer(srv, &recordingAX{seen: seen})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	go func() { _ = srv.Serve(lis) }()
	return lis.Addr().String(), seen
}

func dial(t *testing.T, addr string, opts ...grpc.DialOption) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func spanNamed(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

func TestServerStatsHandlerJoinsCallerTraceAndDropsBaggage(t *testing.T) {
	spans, _ := useTestProviders(t)
	addr, seen := startAX(t)

	const (
		traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
		spanID  = "00f067aa0ba902b7"
	)
	ctx := metadata.AppendToOutgoingContext(context.Background(),
		"traceparent", "00-"+traceID+"-"+spanID+"-01",
		"baggage", "user=alice")
	if _, err := v1alpha1.NewAXClient(dial(t, addr)).ListTasks(ctx, &v1alpha1.ListTasksRequest{}); err != nil {
		t.Fatal(err)
	}

	got := <-seen
	if got.span.TraceID().String() != traceID {
		t.Errorf("handler trace ID = %s, want the caller's %s", got.span.TraceID(), traceID)
	}
	if got.baggage.Len() != 0 {
		t.Errorf("handler saw baggage %q, want none", got.baggage.String())
	}
	server := spanNamed(spans.Ended(), "ax.v1alpha1.AX/ListTasks")
	if server == nil {
		t.Fatalf("no ListTasks server span; got %d spans", len(spans.Ended()))
	}
	if server.SpanKind() != trace.SpanKindServer {
		t.Errorf("span kind = %v, want server", server.SpanKind())
	}
	if !server.Parent().IsRemote() || server.Parent().SpanID().String() != spanID {
		t.Errorf("server span parent = %v, want the caller's remote span %s", server.Parent(), spanID)
	}
}

func TestServerStatsHandlerSkipsUnknownMethods(t *testing.T) {
	spans, reader := useTestProviders(t)
	addr, _ := startAX(t)

	const unknown = "/attacker.Service/Method1234"
	err := dial(t, addr).Invoke(context.Background(), unknown, &emptypb.Empty{}, &emptypb.Empty{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("Invoke(%s) = %v, want Unimplemented", unknown, err)
	}
	for _, s := range spans.Ended() {
		t.Errorf("unexpected span %q for an unknown method", s.Name())
	}
	for _, dp := range histogramPoints(t, reader, "rpc.server.call.duration") {
		t.Errorf("unexpected rpc.server.call.duration point %v for an unknown method", dp.Attributes.ToSlice())
	}
}

func TestClientStatsHandlerPropagatesSpanWithoutBaggage(t *testing.T) {
	spans, reader := useTestProviders(t)
	addr, seen := startAX(t)

	member, err := baggage.NewMember("user", "alice")
	if err != nil {
		t.Fatal(err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatal(err)
	}
	ctx, parent := otel.Tracer("test").Start(baggage.ContextWithBaggage(context.Background(), bag), "reconcile")
	client := v1alpha1.NewAXClient(dial(t, addr, grpc.WithStatsHandler(ClientStatsHandler())))
	if _, err := client.ListTasks(ctx, &v1alpha1.ListTasksRequest{}); err != nil {
		t.Fatal(err)
	}
	parent.End()

	got := <-seen
	if got.span.TraceID() != parent.SpanContext().TraceID() {
		t.Errorf("server trace ID = %s, want the client's %s", got.span.TraceID(), parent.SpanContext().TraceID())
	}
	if got.baggage.Len() != 0 {
		t.Errorf("server saw baggage %q, want none", got.baggage.String())
	}

	var clientSpan, serverSpan sdktrace.ReadOnlySpan
	for _, s := range spans.Ended() {
		switch {
		case s.Name() == "ax.v1alpha1.AX/ListTasks" && s.SpanKind() == trace.SpanKindClient:
			clientSpan = s
		case s.Name() == "ax.v1alpha1.AX/ListTasks" && s.SpanKind() == trace.SpanKindServer:
			serverSpan = s
		}
	}
	if clientSpan == nil || serverSpan == nil {
		t.Fatalf("want a client and a server ListTasks span, got %d spans", len(spans.Ended()))
	}
	if clientSpan.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Errorf("client span parent = %s, want the reconcile span", clientSpan.Parent().SpanID())
	}
	if serverSpan.Parent().SpanID() != clientSpan.SpanContext().SpanID() {
		t.Errorf("server span parent = %s, want the client span %s", serverSpan.Parent().SpanID(), clientSpan.SpanContext().SpanID())
	}

	for _, name := range []string{"rpc.server.call.duration", "rpc.client.call.duration"} {
		points := histogramPoints(t, reader, name)
		if len(points) != 1 {
			t.Fatalf("%s: got %d points, want 1", name, len(points))
		}
		if !slices.Equal(points[0].Bounds, RPCDurationBuckets) {
			t.Errorf("%s bounds = %v, want %v", name, points[0].Bounds, RPCDurationBuckets)
		}
		if v, _ := points[0].Attributes.Value(attribute.Key("rpc.method")); v.AsString() != "ax.v1alpha1.AX/ListTasks" {
			t.Errorf("%s rpc.method = %q, want ax.v1alpha1.AX/ListTasks", name, v.AsString())
		}
	}
}

// histogramPoints collects reader and returns the points of the named
// histogram.
func histogramPoints(t *testing.T, reader *sdkmetric.ManualReader, name string) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var points []metricdata.HistogramDataPoint[float64]
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if h, ok := m.Data.(metricdata.Histogram[float64]); ok && m.Name == name {
				points = append(points, h.DataPoints...)
			}
		}
	}
	return points
}
