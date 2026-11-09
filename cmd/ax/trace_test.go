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

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/emptypb"
)

// traceCaptureServer records the request metadata of every RPC it serves.
type traceCaptureServer struct {
	v1alpha1.UnimplementedAXServer
	headers chan metadata.MD
}

func (s *traceCaptureServer) ListTasks(ctx context.Context, _ *v1alpha1.ListTasksRequest) (*v1alpha1.ListTasksResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.headers <- md
	return &v1alpha1.ListTasksResponse{}, nil
}

// startTraceCaptureServer serves the AX API, and records the metadata of
// streams to unregistered methods too.
func startTraceCaptureServer(t *testing.T) (string, <-chan metadata.MD) {
	t.Helper()
	headers := make(chan metadata.MD, 4)
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		md, _ := metadata.FromIncomingContext(stream.Context())
		headers <- md
		return nil
	}))
	v1alpha1.RegisterAXServer(srv, &traceCaptureServer{headers: headers})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	go func() { _ = srv.Serve(listener) }()
	return listener.Addr().String(), headers
}

// useTrace makes ct the active command trace for the rest of the test.
func useTrace(t *testing.T, ct *commandTrace) {
	t.Helper()
	activeTrace = ct
	t.Cleanup(func() { activeTrace = nil })
}

// discardStdout drops what the command prints.
func discardStdout(t *testing.T) {
	t.Helper()
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = devNull
	t.Cleanup(func() {
		os.Stdout = stdout
		_ = devNull.Close()
	})
}

// clearOTelEnv keeps the developer's OTel settings out of the test, and
// restores the global error handler that startCommandTrace replaces.
func clearOTelEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"OTEL_TRACES_EXPORTER", "OTEL_TRACES_SAMPLER", "OTEL_TRACES_SAMPLER_ARG",
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES",
	} {
		// t.Setenv records the value to restore; the SDK treats a set but
		// empty OTEL_TRACES_SAMPLER as an unknown sampler, so unset it.
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { log.Print(err) }))
	})
}

// keptSpans is an in-memory exporter that keeps its spans after the tracer
// provider shuts down; tracetest.InMemoryExporter clears them on Shutdown.
type keptSpans struct{ *tracetest.InMemoryExporter }

func (keptSpans) Shutdown(context.Context) error { return nil }

func newKeptSpans() keptSpans { return keptSpans{tracetest.NewInMemoryExporter()} }

// failingExporter fails to shut down, as an exporter does when its collector
// is unreachable.
type failingExporter struct{}

func (failingExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (failingExporter) Shutdown(context.Context) error {
	return errors.New("context deadline exceeded")
}

// traceparent returns the fields of the single traceparent header in md.
func traceparent(t *testing.T, md metadata.MD) (traceID, spanID, flags string) {
	t.Helper()
	values := md.Get("traceparent")
	if len(values) != 1 {
		t.Fatalf("expected one traceparent header, got %q", values)
	}
	parts := strings.Split(values[0], "-")
	if len(parts) != 4 || parts[0] != "00" {
		t.Fatalf("malformed traceparent %q", values[0])
	}
	return parts[1], parts[2], parts[3]
}

func TestTraceFlagSamplesCommandAndPrintsTraceID(t *testing.T) {
	clearOTelEnv(t)
	discardStdout(t)
	addr, headers := startTraceCaptureServer(t)

	var stderr bytes.Buffer
	ct := startCommandTrace("get", true, &stderr)
	useTrace(t, ct)
	defer ct.end(nil)

	if err := runGet(addr, "default", []string{"tasks"}); err != nil {
		t.Fatal(err)
	}
	md := <-headers
	traceID, _, flags := traceparent(t, md)
	if want := ct.span.SpanContext().TraceID().String(); traceID != want {
		t.Errorf("traceparent trace ID = %s, want the command trace %s", traceID, want)
	}
	if flags != "01" {
		t.Errorf("traceparent flags = %s, want 01 (sampled) with --trace", flags)
	}
	if got := md.Get("baggage"); len(got) != 0 {
		t.Errorf("expected no baggage header, got %q", got)
	}
	if want := "Trace ID: " + traceID + "\n"; stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestNoTraceWithoutFlag(t *testing.T) {
	clearOTelEnv(t)
	var stderr bytes.Buffer
	if ct := startCommandTrace("get", false, &stderr); ct != nil {
		ct.end(nil)
		t.Fatal("expected no command trace without --trace")
	}
	if stderr.Len() != 0 {
		t.Errorf("expected no output without --trace, got %q", stderr.String())
	}
}

func TestNoTraceparentWithoutCommandTrace(t *testing.T) {
	discardStdout(t)
	addr, headers := startTraceCaptureServer(t)
	useTrace(t, nil)

	if err := runGet(addr, "default", []string{"tasks"}); err != nil {
		t.Fatal(err)
	}
	if got := (<-headers).Get("traceparent"); len(got) != 0 {
		t.Errorf("expected no traceparent without --trace, got %q", got)
	}
}

func TestCommandTraceExportsSpans(t *testing.T) {
	clearOTelEnv(t)
	discardStdout(t)
	addr, headers := startTraceCaptureServer(t)

	exporter := newKeptSpans()
	ct := newCommandTrace("get", exporter, io.Discard)
	useTrace(t, ct)

	ct.startStep("resolve ax-server address")(nil)
	if err := runGet(addr, "default", []string{"tasks"}); err != nil {
		t.Fatal(err)
	}
	_, sentSpanID, _ := traceparent(t, <-headers)
	ct.end(nil)

	byName := map[string]tracetest.SpanStub{}
	var client *tracetest.SpanStub
	spans := exporter.GetSpans()
	for i, s := range spans {
		byName[s.Name] = s
		if s.SpanKind == trace.SpanKindClient && strings.HasSuffix(s.Name, "/ListTasks") {
			client = &spans[i]
		}
	}
	root, ok := byName["ax get"]
	step, stepOK := byName["resolve ax-server address"]
	if !ok || !stepOK || client == nil {
		t.Fatalf("expected the command, resolve, and ListTasks client spans, got %d spans: %+v", len(spans), spans)
	}
	if root.Parent.IsValid() {
		t.Errorf("command span has parent %v, want a root span", root.Parent)
	}
	for _, child := range []tracetest.SpanStub{step, *client} {
		if child.Parent.SpanID() != root.SpanContext.SpanID() {
			t.Errorf("%s parent = %s, want the command span %s", child.Name, child.Parent.SpanID(), root.SpanContext.SpanID())
		}
	}
	if got := client.SpanContext.SpanID().String(); got != sentSpanID {
		t.Errorf("traceparent parent ID = %s, want the client span %s", sentSpanID, got)
	}
	attrs := root.Resource.Set()
	if v, ok := attrs.Value("service.name"); !ok || v.AsString() != "ax" {
		t.Errorf("service.name = %v, want ax", v)
	}
	if v, ok := attrs.Value("telemetry.sdk.language"); !ok || v.AsString() != "go" {
		t.Errorf("telemetry.sdk.language = %v, want go", v)
	}
}

func TestCommandResourceReadsEnv(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("OTEL_SERVICE_NAME", "ax-dev")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment.name=laptop")

	attrs := commandResource(io.Discard).Set()
	if v, _ := attrs.Value("service.name"); v.AsString() != "ax-dev" {
		t.Errorf("service.name = %q, want OTEL_SERVICE_NAME ax-dev", v.AsString())
	}
	if v, _ := attrs.Value("deployment.environment.name"); v.AsString() != "laptop" {
		t.Errorf("deployment.environment.name = %q, want laptop", v.AsString())
	}
}

func TestCommandTraceRecordsError(t *testing.T) {
	exporter := newKeptSpans()
	newCommandTrace("delete", exporter, io.Discard).end(errors.New("task not found"))

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected one span, got %d", len(spans))
	}
	if spans[0].Status.Code != codes.Error || spans[0].Status.Description != "task not found" {
		t.Errorf("status = %+v, want Error with the command error", spans[0].Status)
	}
}

func TestEndWarnsWhenExportFails(t *testing.T) {
	var stderr bytes.Buffer
	newCommandTrace("get", failingExporter{}, &stderr).end(nil)

	if want := "Warning: could not export the command trace: context deadline exceeded\n"; stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestSDKErrorsPrintAsWarnings(t *testing.T) {
	clearOTelEnv(t)
	t.Setenv("OTEL_TRACES_SAMPLER", "bogus")

	var stderr bytes.Buffer
	startCommandTrace("get", true, &stderr).end(nil)

	for line := range strings.Lines(stderr.String()) {
		if !strings.HasPrefix(line, "Trace ID: ") && !strings.HasPrefix(line, "Warning: OpenTelemetry: ") {
			t.Errorf("unexpected stderr line %q", line)
		}
	}
	if !strings.Contains(stderr.String(), "Warning: OpenTelemetry: ") {
		t.Errorf("expected the SDK's sampler error as a warning, got %q", stderr.String())
	}
}

func TestStreamRPCJoinsCommandTrace(t *testing.T) {
	addr, headers := startTraceCaptureServer(t)

	ct := newCommandTrace("logs", nil, io.Discard)
	useTrace(t, ct)
	defer ct.end(nil)

	_, conn, err := getAXClient(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := conn.NewStream(context.Background(), &grpc.StreamDesc{ServerStreams: true}, "/ax.v1alpha1.AX/StreamTaskLogs")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if err := stream.RecvMsg(&emptypb.Empty{}); !errors.Is(err, io.EOF) {
		t.Fatalf("RecvMsg = %v, want io.EOF", err)
	}
	traceID, _, _ := traceparent(t, <-headers)
	if want := ct.span.SpanContext().TraceID().String(); traceID != want {
		t.Errorf("stream traceparent trace ID = %s, want %s", traceID, want)
	}
}

func TestResolveExporters(t *testing.T) {
	known := []string{"otlp"}
	def := []string{"otlp"}
	tests := []struct {
		value   string
		want    []string
		wantErr bool
	}{
		{value: "", want: def},
		{value: "  ", want: def},
		{value: "otlp", want: []string{"otlp"}},
		{value: " OTLP ", want: []string{"otlp"}},
		{value: "none", want: nil},
		{value: " None ", want: nil},
		{value: "otlp,console", want: []string{"otlp"}, wantErr: true},
		{value: "otlp,", want: []string{"otlp"}, wantErr: true},
		{value: "console", want: def, wantErr: true},
		{value: "none,otlp", want: def, wantErr: true},
		{value: "none,console", want: nil, wantErr: true},
	}
	for _, tt := range tests {
		got, err := resolveExporters("OTEL_TRACES_EXPORTER", tt.value, known, def)
		if !slices.Equal(got, tt.want) || (err != nil) != tt.wantErr {
			t.Errorf("resolveExporters(%q) = %q, %v; want %q, error %v", tt.value, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestFinish(t *testing.T) {
	t.Run("remote exit code", func(t *testing.T) {
		exporter := newKeptSpans()
		ct := newCommandTrace("ssh", exporter, io.Discard)
		var stderr bytes.Buffer

		if code := finish(ct, exitCodeError{code: 3}, &stderr); code != 3 {
			t.Errorf("finish = %d, want the remote exit code 3", code)
		}
		if stderr.Len() != 0 {
			t.Errorf("expected no error message for a remote exit code, got %q", stderr.String())
		}
		spans := exporter.GetSpans()
		if len(spans) != 1 || spans[0].Status.Code != codes.Error {
			t.Errorf("expected the command span to be ended and exported with an error before finish returns, got %+v", spans)
		}
	})
	t.Run("error", func(t *testing.T) {
		var stderr bytes.Buffer
		if code := finish(nil, errors.New("boom"), &stderr); code != 1 {
			t.Errorf("finish = %d, want 1", code)
		}
		if stderr.String() != "Error: boom\n" {
			t.Errorf("stderr = %q, want %q", stderr.String(), "Error: boom\n")
		}
	})
	t.Run("success", func(t *testing.T) {
		if code := finish(nil, nil, io.Discard); code != 0 {
			t.Errorf("finish = %d, want 0", code)
		}
	})
}
