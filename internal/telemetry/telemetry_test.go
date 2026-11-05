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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/grpc"
)

// fakeCollector is an in-process OTLP/gRPC collector that keeps what it
// receives.
type fakeCollector struct {
	coltracepb.UnimplementedTraceServiceServer
	collogspb.UnimplementedLogsServiceServer

	mu    sync.Mutex
	spans []*coltracepb.ExportTraceServiceRequest
	logs  []*collogspb.ExportLogsServiceRequest
}

func (c *fakeCollector) Export(_ context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spans = append(c.spans, req)
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

// logsService adapts fakeCollector to the logs service, whose Export method
// has the same name as the trace service's.
type logsService struct {
	collogspb.UnimplementedLogsServiceServer
	c *fakeCollector
}

func (s logsService) Export(_ context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	s.c.logs = append(s.c.logs, req)
	return &collogspb.ExportLogsServiceResponse{}, nil
}

// startCollector starts a fakeCollector and points the OTLP exporters at it.
func startCollector(t *testing.T) *fakeCollector {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &fakeCollector{}
	srv := grpc.NewServer()
	coltracepb.RegisterTraceServiceServer(srv, c)
	collogspb.RegisterLogsServiceServer(srv, logsService{c: c})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+lis.Addr().String())
	return c
}

// initForTest runs Init and shuts the providers down when the test ends.
func initForTest(t *testing.T) *Providers {
	t.Helper()
	p, err := Init(context.Background(), "ax-server")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	return p
}

func resourceAttr(attrs []*commonpb.KeyValue, key string) string {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

func TestInitExportsSpansAndLogsOverOTLP(t *testing.T) {
	c := startCollector(t)
	t.Setenv("OTEL_TRACES_SAMPLER", "always_on")
	p := initForTest(t)

	_, span := otel.Tracer("test").Start(context.Background(), "test-span")
	span.End()
	var rec otellog.Record
	rec.SetEventName("ax.test.event")
	rec.SetBody(attribute.StringValue("hello"))
	global.GetLoggerProvider().Logger("test").Emit(context.Background(), rec)

	// Shutdown flushes both batchers.
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.spans) == 0 || len(c.spans[0].GetResourceSpans()) == 0 {
		t.Fatal("collector received no spans")
	}
	rs := c.spans[0].GetResourceSpans()[0]
	if got := resourceAttr(rs.GetResource().GetAttributes(), "service.name"); got != "ax-server" {
		t.Errorf("span resource service.name = %q, want ax-server", got)
	}
	if name := rs.GetScopeSpans()[0].GetSpans()[0].GetName(); name != "test-span" {
		t.Errorf("span name = %q, want test-span", name)
	}
	if len(c.logs) == 0 || len(c.logs[0].GetResourceLogs()) == 0 {
		t.Fatal("collector received no log records")
	}
	lr := c.logs[0].GetResourceLogs()[0].GetScopeLogs()[0].GetLogRecords()[0]
	if lr.GetEventName() != "ax.test.event" || lr.GetBody().GetStringValue() != "hello" {
		t.Errorf("log record = %v, want event ax.test.event with body hello", lr)
	}
}

func TestInitHonorsExporterNone(t *testing.T) {
	c := startCollector(t)
	t.Setenv("OTEL_TRACES_SAMPLER", "always_on")
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	p := initForTest(t)
	if p.Logger != nil {
		t.Error("Logger provider built although OTEL_LOGS_EXPORTER=none")
	}

	// Spans are still sampled, so trace context keeps flowing, but not sent.
	_, span := otel.Tracer("test").Start(context.Background(), "unexported")
	if !span.SpanContext().IsSampled() {
		t.Error("span not sampled with OTEL_TRACES_EXPORTER=none")
	}
	span.End()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.spans) != 0 || len(c.logs) != 0 {
		t.Errorf("collector received %d span and %d log exports, want none", len(c.spans), len(c.logs))
	}
}

func TestDefaultSamplerFollowsParentAndSamplesTenthOfRoots(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	initForTest(t)
	tracer := otel.Tracer("test")

	const roots = 4000
	sampled := 0
	for range roots {
		_, span := tracer.Start(context.Background(), "root")
		if span.SpanContext().IsSampled() {
			sampled++
		}
		span.End()
	}
	// 10% of 4000 is 400, with a standard deviation of about 19.
	if sampled < 300 || sampled > 500 {
		t.Errorf("%d of %d root spans sampled, want about %d", sampled, roots, int(roots*DefaultSampleRatio))
	}

	for _, parentSampled := range []bool{true, false} {
		flags := trace.TraceFlags(0)
		if parentSampled {
			flags = trace.FlagsSampled
		}
		parent := trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}, TraceFlags: flags, Remote: true,
		})
		_, span := tracer.Start(trace.ContextWithRemoteSpanContext(context.Background(), parent), "child")
		if got := span.SpanContext().IsSampled(); got != parentSampled {
			t.Errorf("child of a parent with sampled=%v has sampled=%v", parentSampled, got)
		}
		span.End()
	}
}

func TestPropagatorCarriesTraceContextOnly(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	initForTest(t)
	got := strings.Join(otel.GetTextMapPropagator().Fields(), ",")
	if got != "traceparent,tracestate" {
		t.Errorf("propagated fields = %s, want traceparent,tracestate (no baggage)", got)
	}
}

func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestMetricsHandlerServesInstrumentsWithCardinalityCap(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	p := initForTest(t)

	counter, err := otel.Meter("test").Int64Counter("ax.test.requests")
	if err != nil {
		t.Fatal(err)
	}
	for i := range CardinalityLimit * 3 {
		counter.Add(context.Background(), 1, metric.WithAttributes(attribute.String("key", fmt.Sprint(i))))
	}

	body := scrape(t, p.MetricsHandler())
	series := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "ax_test_requests_total{") {
			series++
		}
	}
	// The limit counts the overflow series, so the 300 sets become 100 series.
	if series != CardinalityLimit {
		t.Errorf("ax_test_requests_total has %d series, want %d", series, CardinalityLimit)
	}
	for _, want := range []string{`otel_metric_overflow="true"`, `service_name="ax-server"`, "go_goroutines", "process_cpu_seconds_total"} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics output has no %s", want)
		}
	}
}

func TestExporterSelected(t *testing.T) {
	tests := []struct {
		val  string
		want bool
	}{
		{"", true},
		{"otlp", true},
		{" OTLP ", true},
		{"none", false},
		{"otlp,none", false},
		{"console", false},
		{"console,otlp", true},
	}
	for _, tt := range tests {
		t.Setenv("OTEL_TEST_EXPORTER", tt.val)
		if got := exporterSelected(context.Background(), "OTEL_TEST_EXPORTER"); got != tt.want {
			t.Errorf("exporterSelected(%q) = %v, want %v", tt.val, got, tt.want)
		}
	}
}

func TestLogHandlerAddsTraceAndSpanIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewLogHandler(slog.NewJSONHandler(&buf, nil)))
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x0a, 0xf7},
		SpanID:     trace.SpanID{0x00, 0xf0, 0x67},
		TraceFlags: trace.FlagsSampled,
	})

	logger.InfoContext(trace.ContextWithSpanContext(context.Background(), sc), "with span", "k", "v")
	logger.With("component", "test").InfoContext(trace.ContextWithSpanContext(context.Background(), sc), "with attrs")
	logger.InfoContext(context.Background(), "without span")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d log lines, want 3:\n%s", len(lines), buf.String())
	}
	for i, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d is not JSON: %v\n%s", i, err, line)
		}
		if i == 2 {
			if _, ok := rec["trace_id"]; ok {
				t.Errorf("line without a span has trace_id: %s", line)
			}
			continue
		}
		if rec["trace_id"] != sc.TraceID().String() || rec["span_id"] != sc.SpanID().String() {
			t.Errorf("line %d trace_id=%v span_id=%v, want %s %s", i, rec["trace_id"], rec["span_id"], sc.TraceID(), sc.SpanID())
		}
	}
	if !strings.Contains(lines[1], `"component":"test"`) {
		t.Errorf("WithAttrs attribute lost: %s", lines[1])
	}
}
