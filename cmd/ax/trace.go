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
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
)

const (
	// traceShutdownTimeout bounds how long the CLI waits to export its spans.
	traceShutdownTimeout = 2 * time.Second

	tracesExporterEnv = "OTEL_TRACES_EXPORTER"
	exporterOTLP      = "otlp"
	exporterNone      = "none"
)

// commandTrace is the root span of one `ax --trace` command. Every RPC the
// command makes joins its trace and sends a sampled W3C traceparent, so
// ax-server and Substrate, which sample ParentBased, record the whole request.
type commandTrace struct {
	tp     *sdktrace.TracerProvider
	span   trace.Span
	stderr io.Writer
}

// activeTrace is the trace of the running command. It is nil without --trace:
// the CLI then creates no tracer, sends no trace context, and leaves the
// sampling decision to ax-server.
var activeTrace *commandTrace

// startCommandTrace starts the root span for cmd when force (--trace) is set,
// prints its trace ID to stderr, and reports OpenTelemetry errors there as
// warnings. Spans are exported over OTLP only when an OTLP endpoint is set in
// the environment. It returns nil without force.
func startCommandTrace(cmd string, force bool, stderr io.Writer) *commandTrace {
	if !force {
		return nil
	}
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		fmt.Fprintf(stderr, "Warning: OpenTelemetry: %v\n", err)
	}))
	t := newCommandTrace(cmd, envSpanExporter(stderr), stderr)
	fmt.Fprintf(stderr, "Trace ID: %s\n", t.span.SpanContext().TraceID())
	return t
}

// newCommandTrace starts a sampled root span named "ax <cmd>". A nil exporter
// keeps spans local; the trace context still reaches ax-server.
func newCommandTrace(cmd string, exporter sdktrace.SpanExporter, stderr io.Writer) *commandTrace {
	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(commandResource(stderr)),
	}
	if exporter != nil {
		opts = append(opts, sdktrace.WithBatcher(exporter))
	}
	tp := sdktrace.NewTracerProvider(opts...)
	_, span := tp.Tracer("github.com/google/ax/cmd/ax").Start(context.Background(), "ax "+cmd)
	return &commandTrace{tp: tp, span: span, stderr: stderr}
}

// commandResource describes the CLI the way ax-server describes itself:
// service.name "ax", the SDK attributes, and OTEL_SERVICE_NAME and
// OTEL_RESOURCE_ATTRIBUTES on top.
func commandResource(stderr io.Writer) *resource.Resource {
	res, err := resource.New(context.Background(),
		resource.WithTelemetrySDK(),
		resource.WithSchemaURL(semconv.SchemaURL),
		resource.WithAttributes(semconv.ServiceName("ax")),
		resource.WithFromEnv(),
	)
	if err != nil {
		fmt.Fprintf(stderr, "Warning: OpenTelemetry resource: %v\n", err)
	}
	if res == nil {
		res = resource.Empty()
	}
	return res
}

// envSpanExporter returns an OTLP gRPC exporter configured from the standard
// OTEL_EXPORTER_OTLP_* variables, or nil when no endpoint is set or
// OTEL_TRACES_EXPORTER turns OTLP off.
func envSpanExporter(stderr io.Writer) sdktrace.SpanExporter {
	value := os.Getenv(tracesExporterEnv)
	names, err := resolveExporters(tracesExporterEnv, value, []string{exporterOTLP}, []string{exporterOTLP})
	if err != nil {
		fmt.Fprintf(stderr, "Warning: %v; using %q\n", err, strings.Join(names, ","))
	}
	if !slices.Contains(names, exporterOTLP) {
		return nil
	}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return nil
	}
	exporter, err := otlptracegrpc.New(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "Warning: not exporting the command trace: %v\n", err)
		return nil
	}
	return exporter
}

// resolveExporters reads value as a comma-separated exporter list, with the
// rules ax-server uses for the same variables. Names are trimmed and
// lowercased; empty and unknown names are skipped with an error, and the rest
// apply. A value that is empty, names no known exporter, or lists none with an
// exporter keeps def. The returned names are the ones to use even when err is
// not nil.
func resolveExporters(env, value string, known, def []string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return slices.Clone(def), nil
	}
	var got, skipped []string
	none := false
	for item := range strings.SplitSeq(value, ",") {
		switch name := strings.ToLower(strings.TrimSpace(item)); {
		case name == exporterNone:
			none = true
		case slices.Contains(known, name):
			if !slices.Contains(got, name) {
				got = append(got, name)
			}
		default:
			skipped = append(skipped, name)
		}
	}
	var skipErr error
	if len(skipped) > 0 {
		skipErr = fmt.Errorf("%s: skipped empty or unknown exporters %q", env, skipped)
	}
	switch {
	case none && len(got) > 0:
		return slices.Clone(def), fmt.Errorf("%s %q lists none with an exporter", env, value)
	case none:
		return nil, skipErr
	case len(got) == 0:
		return slices.Clone(def), fmt.Errorf("%s %q names no known exporter", env, value)
	}
	return got, skipErr
}

// dialOptions instruments a client connection: each RPC becomes a client span
// under the command span and carries only the traceparent header (no baggage).
func (t *commandTrace) dialOptions() []grpc.DialOption {
	if t == nil {
		return nil
	}
	return []grpc.DialOption{
		grpc.WithStatsHandler(otelgrpc.NewClientHandler(
			otelgrpc.WithTracerProvider(t.tp),
			otelgrpc.WithMeterProvider(metricnoop.NewMeterProvider()),
			otelgrpc.WithPropagators(propagation.TraceContext{}),
		)),
		grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(t.parent(ctx), method, req, reply, cc, opts...)
		}),
		grpc.WithChainStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			return streamer(t.parent(ctx), desc, cc, method, opts...)
		}),
	}
}

// parent puts the command span in ctx unless ctx already carries a span. The
// commands build RPC contexts from context.Background, so without this each
// RPC would start a trace of its own.
func (t *commandTrace) parent(ctx context.Context) context.Context {
	if trace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	return trace.ContextWithSpan(ctx, t.span)
}

// startStep starts a child span of the command for one local step, such as
// resolving the ax-server address, and returns the function that ends it.
func (t *commandTrace) startStep(name string) func(error) {
	if t == nil {
		return func(error) {}
	}
	_, span := t.tp.Tracer("github.com/google/ax/cmd/ax").Start(trace.ContextWithSpan(context.Background(), t.span), name)
	return func(err error) {
		recordError(span, err)
		span.End()
	}
}

// end records err on the command span, ends it, and flushes any exporter.
func (t *commandTrace) end(err error) {
	if t == nil {
		return
	}
	recordError(t.span, err)
	t.span.End()
	ctx, cancel := context.WithTimeout(context.Background(), traceShutdownTimeout)
	defer cancel()
	if err := t.tp.Shutdown(ctx); err != nil {
		fmt.Fprintf(t.stderr, "Warning: could not export the command trace: %v\n", err)
	}
}

func recordError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// exitCodeError makes the CLI exit with code without printing an error, as
// when the command run by `ax ssh` exits non-zero.
type exitCodeError struct {
	code int
}

func (e exitCodeError) Error() string {
	return fmt.Sprintf("remote command exited with code %d", e.code)
}

// finish ends the command trace, reports err, and returns the exit code. The
// trace ends first, so a command that fails still exports its spans.
func finish(t *commandTrace, err error, stderr io.Writer) int {
	t.end(err)
	if err == nil {
		return 0
	}
	var exitErr exitCodeError
	if errors.As(err, &exitErr) {
		return exitErr.code
	}
	fmt.Fprintf(stderr, "Error: %v\n", err)
	return 1
}
