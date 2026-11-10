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
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/grpc"
)

// fakeCollector is an in-process OTLP/gRPC collector that keeps what it
// receives. It serves the trace service itself; logsService and
// metricsService serve the others, whose Export methods share the name.
type fakeCollector struct {
	coltracepb.UnimplementedTraceServiceServer

	mu      sync.Mutex
	spans   []*coltracepb.ExportTraceServiceRequest
	logs    []*collogspb.ExportLogsServiceRequest
	metrics []*colmetricspb.ExportMetricsServiceRequest
}

func (c *fakeCollector) Export(_ context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spans = append(c.spans, req)
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

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

type metricsService struct {
	colmetricspb.UnimplementedMetricsServiceServer
	c *fakeCollector
}

func (s metricsService) Export(_ context.Context, req *colmetricspb.ExportMetricsServiceRequest) (*colmetricspb.ExportMetricsServiceResponse, error) {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	s.c.metrics = append(s.c.metrics, req)
	return &colmetricspb.ExportMetricsServiceResponse{}, nil
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
	colmetricspb.RegisterMetricsServiceServer(srv, metricsService{c: c})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+lis.Addr().String())
	return c
}

// noExport turns off every OTLP exporter, for tests without a collector.
func noExport(t *testing.T) {
	t.Helper()
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_METRICS_EXPORTER", "prometheus")
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
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

// captureLogs points slog.Default() at a JSON buffer until the test ends.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// stdoutEvents returns the log lines in buf that carry an event attribute.
func stdoutEvents(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, line)
		}
		if _, ok := rec["event"]; ok {
			events = append(events, rec)
		}
	}
	return events
}

var testEvent = Event{Name: "ax.test.event", Body: "Test event", Severity: otellog.SeverityInfo}

func resourceAttr(attrs []*commonpb.KeyValue, key string) string {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

func TestInitExportsAllSignalsOverOTLP(t *testing.T) {
	c := startCollector(t)
	t.Setenv("OTEL_TRACES_SAMPLER", "always_on")
	t.Setenv("OTEL_LOGS_EXPORTER", "otlp")
	buf := captureLogs(t)
	p := initForTest(t)

	_, span := otel.Tracer("test").Start(context.Background(), "test-span")
	span.End()
	counter, err := otel.Meter("test").Int64Counter("ax.test.requests")
	if err != nil {
		t.Fatal(err)
	}
	counter.Add(context.Background(), 1)
	Emit(context.Background(), testEvent, slog.String("ax.task.id", "t-1"), slog.Int("count", 3))

	// Shutdown flushes the batchers and runs a last metrics export.
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

	if len(c.metrics) == 0 || len(c.metrics[0].GetResourceMetrics()) == 0 {
		t.Fatal("collector received no metrics")
	}
	var names []string
	for _, rm := range c.metrics[0].GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				names = append(names, m.GetName())
			}
		}
	}
	if !slices.Contains(names, "ax.test.requests") {
		t.Errorf("pushed metrics = %v, want ax.test.requests", names)
	}

	if len(c.logs) == 0 || len(c.logs[0].GetResourceLogs()) == 0 {
		t.Fatal("collector received no log records")
	}
	lr := c.logs[0].GetResourceLogs()[0].GetScopeLogs()[0].GetLogRecords()[0]
	if lr.GetEventName() != testEvent.Name || lr.GetBody().GetStringValue() != testEvent.Body {
		t.Errorf("log record = %v, want event %s with body %q", lr, testEvent.Name, testEvent.Body)
	}
	if got := resourceAttr(lr.GetAttributes(), "ax.task.id"); got != "t-1" {
		t.Errorf("log record ax.task.id = %q, want t-1", got)
	}
	// Without console, an event sent over OTLP is not also written to stdout.
	if events := stdoutEvents(t, buf); len(events) != 0 {
		t.Errorf("event also written to stdout without console: %v", events)
	}
}

func TestInitHonorsExporterNone(t *testing.T) {
	c := startCollector(t)
	t.Setenv("OTEL_TRACES_SAMPLER", "always_on")
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
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
	counter, err := otel.Meter("test").Int64Counter("ax.test.requests")
	if err != nil {
		t.Fatal(err)
	}
	counter.Add(context.Background(), 1)
	// /metrics still serves with the push off.
	if body := scrape(t, p.MetricsHandler()); !strings.Contains(body, "ax_test_requests_total") {
		t.Error("/metrics has no ax_test_requests_total with OTEL_METRICS_EXPORTER=none")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.spans) != 0 || len(c.metrics) != 0 || len(c.logs) != 0 {
		t.Errorf("collector received %d span, %d metric, and %d log exports, want none", len(c.spans), len(c.metrics), len(c.logs))
	}
}

func TestEmitWritesEventsToStdoutByDefault(t *testing.T) {
	c := startCollector(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	buf := captureLogs(t)
	p := initForTest(t)
	if p.Logger != nil {
		t.Error("Logger provider built with OTEL_LOGS_EXPORTER unset; the default is none")
	}

	Emit(context.Background(), testEvent, slog.String("ax.task.id", "t-1"))
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	events := stdoutEvents(t, buf)
	if len(events) != 1 {
		t.Fatalf("got %d stdout events, want 1:\n%s", len(events), buf.String())
	}
	if ev := events[0]; ev["event"] != testEvent.Name || ev["msg"] != testEvent.Body || ev["level"] != "INFO" || ev["ax.task.id"] != "t-1" {
		t.Errorf("stdout event = %v", ev)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.logs) != 0 {
		t.Errorf("collector received %d log exports, want none", len(c.logs))
	}
}

func TestEmitWithConsoleWritesBoth(t *testing.T) {
	c := startCollector(t)
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	t.Setenv("OTEL_LOGS_EXPORTER", "otlp,console")
	buf := captureLogs(t)
	p := initForTest(t)

	Emit(context.Background(), testEvent)
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if events := stdoutEvents(t, buf); len(events) != 1 {
		t.Errorf("got %d stdout events, want 1:\n%s", len(events), buf.String())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.logs) != 1 {
		t.Errorf("collector received %d log exports, want 1", len(c.logs))
	}
}

// sampledShare starts n root spans and returns the share sampled.
func sampledShare(n int) float64 {
	tracer := otel.Tracer("test")
	sampled := 0
	for range n {
		_, span := tracer.Start(context.Background(), "root")
		if span.SpanContext().IsSampled() {
			sampled++
		}
		span.End()
	}
	return float64(sampled) / float64(n)
}

func TestDefaultSamplerFollowsParentAndSamplesTenthOfRoots(t *testing.T) {
	noExport(t)
	initForTest(t)

	// 10% of 4000 is 400, with a standard deviation of about 19.
	if got := sampledShare(4000); got < 0.075 || got > 0.125 {
		t.Errorf("%.3f of root spans sampled, want about %.1f", got, DefaultSampleRatio)
	}

	tracer := otel.Tracer("test")
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

func TestInvalidSamplerSettingKeepsDefault(t *testing.T) {
	for _, tt := range []struct{ sampler, arg string }{
		{"parentbased_traceidratoi", "0.1"}, // typo
		{"parentbased_traceidratio", ""},
		{"parentbased_traceidratio", "10"},
	} {
		t.Run(tt.sampler+"="+tt.arg, func(t *testing.T) {
			noExport(t)
			t.Setenv("OTEL_TRACES_SAMPLER", tt.sampler)
			t.Setenv("OTEL_TRACES_SAMPLER_ARG", tt.arg)
			initForTest(t)
			if got := sampledShare(2000); got < 0.065 || got > 0.135 {
				t.Errorf("%.3f of root spans sampled, want about %.1f", got, DefaultSampleRatio)
			}
		})
	}
}

func TestParseSampler(t *testing.T) {
	def := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(DefaultSampleRatio))
	tests := []struct {
		name, arg string
		nameSet   bool
		argSet    bool
		want      string // the sampler's Description
		wantErr   bool
	}{
		{name: "", want: def.Description()},
		{name: " ", nameSet: true, want: def.Description()},
		{name: "always_on", nameSet: true, want: "AlwaysOnSampler"},
		{name: "ALWAYS_OFF", nameSet: true, want: "AlwaysOffSampler"},
		{name: "traceidratio", nameSet: true, arg: "0.5", argSet: true, want: "TraceIDRatioBased{0.5}"},
		{name: "parentbased_traceidratio", nameSet: true, arg: " 0.25 ", argSet: true, want: sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.25)).Description()},
		{name: "parentbased_always_on", nameSet: true, want: sdktrace.ParentBased(sdktrace.AlwaysSample()).Description()},
		{name: "parentbased_always_off", nameSet: true, want: sdktrace.ParentBased(sdktrace.NeverSample()).Description()},
		{name: "parentbased_traceidratoi", nameSet: true, arg: "0.1", argSet: true, want: def.Description(), wantErr: true},
		{name: "jaeger_remote", nameSet: true, want: def.Description(), wantErr: true},
		{name: "traceidratio", nameSet: true, want: def.Description(), wantErr: true},
		{name: "traceidratio", nameSet: true, arg: "half", argSet: true, want: def.Description(), wantErr: true},
		{name: "traceidratio", nameSet: true, arg: "-0.1", argSet: true, want: def.Description(), wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseSampler(tt.name, tt.nameSet, tt.arg, tt.argSet, def)
		if got.Description() != tt.want || (err != nil) != tt.wantErr {
			t.Errorf("parseSampler(%q, %q) = %s, %v; want %s, error %v", tt.name, tt.arg, got.Description(), err, tt.want, tt.wantErr)
		}
	}
}

func TestPropagatorCarriesTraceContextOnly(t *testing.T) {
	noExport(t)
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

// countSeries adds n attribute sets to a counter and returns the number of
// series /metrics serves for it, and the body.
func countSeries(t *testing.T, p *Providers, n int) (int, string) {
	t.Helper()
	counter, err := otel.Meter("test").Int64Counter("ax.test.requests")
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		counter.Add(context.Background(), 1, metric.WithAttributes(attribute.String("key", fmt.Sprint(i))))
	}
	body := scrape(t, p.MetricsHandler())
	series := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "ax_test_requests_total{") {
			series++
		}
	}
	return series, body
}

func TestMetricsHandlerServesInstrumentsAndRuntimeMetrics(t *testing.T) {
	noExport(t)
	p := initForTest(t)

	// 300 sets stay under the SDK's default limit of 2000.
	series, body := countSeries(t, p, 300)
	if series != 300 {
		t.Errorf("ax_test_requests_total has %d series, want 300", series)
	}
	for _, want := range []string{`service_name="ax-server"`, "go_goroutines", "process_cpu_seconds_total"} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics output has no %s", want)
		}
	}
	if strings.Contains(body, "otel_metric_overflow") {
		t.Error("/metrics has an overflow series under the default cardinality limit")
	}
}

func TestCardinalityLimitFollowsEnvironment(t *testing.T) {
	noExport(t)
	t.Setenv("OTEL_GO_X_CARDINALITY_LIMIT", "100")
	p := initForTest(t)

	// The limit counts the overflow series, so the 300 sets become 100 series.
	series, body := countSeries(t, p, 300)
	if series != 100 {
		t.Errorf("ax_test_requests_total has %d series, want 100", series)
	}
	if !strings.Contains(body, `otel_metric_overflow="true"`) {
		t.Error("/metrics has no overflow series")
	}
}

func TestResolveExporters(t *testing.T) {
	known := []string{exporterOTLP, exporterConsole}
	def := exporters{exporterOTLP: true}
	tests := []struct {
		val     string
		want    string
		wantErr bool
	}{
		{"", "otlp", false},
		{" ", "otlp", false},
		{"otlp", "otlp", false},
		{" OTLP ", "otlp", false},
		{"console", "console", false},
		{"otlp,console", "console,otlp", false},
		{"none", "none", false},
		{"none,", "none", true},
		{"otlp,none", "otlp", true}, // contradictory: keep the default
		{"otpl", "otlp", true},      // typo: keep the default
		{"zipkin", "otlp", true},
		{"zipkin,console", "console", true},
	}
	for _, tt := range tests {
		got, err := resolveExporters("OTEL_TEST_EXPORTER", tt.val, known, def)
		if got.String() != tt.want || (err != nil) != tt.wantErr {
			t.Errorf("resolveExporters(%q) = %s, %v; want %s, error %v", tt.val, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestAttrValueMatchesJSONKinds(t *testing.T) {
	ts := time.Date(2026, 11, 6, 21, 0, 0, 123, time.UTC)
	tests := []struct {
		in   slog.Value
		want attribute.Value
	}{
		{slog.StringValue("s"), attribute.StringValue("s")},
		{slog.IntValue(-3), attribute.Int64Value(-3)},
		{slog.Uint64Value(math.MaxUint64), attribute.Int64Value(math.MaxInt64)},
		{slog.Float64Value(1.5), attribute.Float64Value(1.5)},
		{slog.BoolValue(true), attribute.BoolValue(true)},
		{slog.DurationValue(time.Second), attribute.Int64Value(int64(time.Second))},
		{slog.TimeValue(ts), attribute.StringValue("2026-11-06T21:00:00.000000123Z")},
		{slog.GroupValue(slog.String("k", "v")), attribute.MapValue(attribute.String("k", "v"))},
	}
	for _, tt := range tests {
		if got := attrValue(tt.in); got != tt.want {
			t.Errorf("attrValue(%v) = %v, want %v", tt.in, got.Emit(), tt.want.Emit())
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
		if rec["trace_id"] != sc.TraceID().String() || rec["span_id"] != sc.SpanID().String() || rec["trace_flags"] != "01" {
			t.Errorf("line %d trace_id=%v span_id=%v trace_flags=%v, want %s %s 01", i, rec["trace_id"], rec["span_id"], rec["trace_flags"], sc.TraceID(), sc.SpanID())
		}
	}
	if !strings.Contains(lines[1], `"component":"test"`) {
		t.Errorf("WithAttrs attribute lost: %s", lines[1])
	}
}
