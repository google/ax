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
	"sync"
	"time"

	"github.com/google/ax/internal/telemetry/otelenv"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
)

const (
	// traceShutdownTimeout bounds how long the CLI waits to export its spans.
	traceShutdownTimeout = 2 * time.Second
	// interruptFlushTimeout bounds the export when the user presses Ctrl-C
	// twice to leave a hung ax ssh session, which must end at once.
	interruptFlushTimeout = 200 * time.Millisecond

	tracerName            = "github.com/google/ax/cmd/ax"
	tracesExporterEnv     = "OTEL_TRACES_EXPORTER"
	otlpEndpointEnv       = "OTEL_EXPORTER_OTLP_ENDPOINT"
	otlpTracesEndpointEnv = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
)

// commandTrace is the root span of one `ax --trace` command. Every RPC the
// command makes joins its trace and sends a sampled W3C traceparent, so
// ax-server and Substrate, which sample ParentBased, can record the whole
// request.
type commandTrace struct {
	tp     *sdktrace.TracerProvider
	span   trace.Span
	stderr io.Writer

	// exporter is nil when the spans stay local. Otherwise spans keeps the
	// ended spans until end exports them, and endpoint names the collector
	// in a warning.
	exporter sdktrace.SpanExporter
	spans    *endedSpans
	endpoint string

	once sync.Once
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
	exporter, endpoint := envSpanExporter(stderr)
	t := newCommandTrace(cmd, exporter, stderr)
	t.endpoint = endpoint
	fmt.Fprintf(stderr, "Trace ID: %s\n", t.span.SpanContext().TraceID())
	return t
}

// newCommandTrace starts a sampled root span named "ax <cmd>". A nil exporter
// keeps spans local; the trace context still reaches ax-server.
func newCommandTrace(cmd string, exporter sdktrace.SpanExporter, stderr io.Writer) *commandTrace {
	t := &commandTrace{stderr: stderr, exporter: exporter}
	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(commandResource(stderr)),
	}
	if exporter != nil {
		t.spans = &endedSpans{}
		opts = append(opts, sdktrace.WithSpanProcessor(t.spans))
	}
	t.tp = sdktrace.NewTracerProvider(opts...)
	_, t.span = t.tp.Tracer(tracerName).Start(context.Background(), "ax "+cmd)
	return t
}

// commandResource describes the CLI with the rules ax-server uses:
// service.name "ax", the SDK attributes, and OTEL_SERVICE_NAME and
// OTEL_RESOURCE_ATTRIBUTES on top.
func commandResource(stderr io.Writer) *resource.Resource {
	res, err := otelenv.Resource(context.Background(), "ax")
	if err != nil {
		fmt.Fprintf(stderr, "Warning: OpenTelemetry resource: %v\n", err)
	}
	if res == nil {
		res = resource.Empty()
	}
	return res
}

// envSpanExporter returns an OTLP gRPC exporter configured from the standard
// OTEL_EXPORTER_OTLP_* variables, and the endpoint it sends to. It returns no
// exporter when no endpoint is set or OTEL_TRACES_EXPORTER, read with
// ax-server's rules, turns OTLP off.
//
// The exporter does not retry. Its first retry would come after 5 s, later
// than the CLI waits, so retrying only delays the warning and hides the real
// error behind a timeout.
func envSpanExporter(stderr io.Writer) (sdktrace.SpanExporter, string) {
	value := os.Getenv(tracesExporterEnv)
	selected, err := otelenv.ResolveExporters(tracesExporterEnv, value,
		[]string{otelenv.ExporterOTLP}, otelenv.Exporters{otelenv.ExporterOTLP: true})
	if err != nil {
		fmt.Fprintf(stderr, "Warning: %v; using %q\n", err, selected.String())
	}
	if !selected.Has(otelenv.ExporterOTLP) {
		return nil, ""
	}
	endpoint := os.Getenv(otlpTracesEndpointEnv)
	if endpoint == "" {
		endpoint = os.Getenv(otlpEndpointEnv)
	}
	if endpoint == "" {
		return nil, ""
	}
	exporter, err := otlptracegrpc.New(context.Background(),
		otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{Enabled: false}))
	if err != nil {
		fmt.Fprintf(stderr, "Warning: not exporting the command trace to %s: %v\n", endpoint, err)
		return nil, ""
	}
	return exporter, endpoint
}

// endedSpans keeps the ended spans of a command, so that end exports them in
// one call and gets the export error back. A batch processor would pass that
// error to the OpenTelemetry error handler instead, and report only a timeout
// from Shutdown.
type endedSpans struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (b *endedSpans) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

func (b *endedSpans) OnEnd(s sdktrace.ReadOnlySpan) {
	if !s.SpanContext().IsSampled() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.spans = append(b.spans, s)
}

func (b *endedSpans) Shutdown(context.Context) error   { return nil }
func (b *endedSpans) ForceFlush(context.Context) error { return nil }

// take returns the kept spans and forgets them.
func (b *endedSpans) take() []sdktrace.ReadOnlySpan {
	b.mu.Lock()
	defer b.mu.Unlock()
	spans := b.spans
	b.spans = nil
	return spans
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
	_, span := t.tp.Tracer(tracerName).Start(trace.ContextWithSpan(context.Background(), t.span), name)
	return func(err error) {
		recordError(span, err)
		span.End()
	}
}

// end records err on the command span, ends it, and exports the command's
// spans, waiting at most traceShutdownTimeout.
func (t *commandTrace) end(err error) {
	t.endWithin(err, traceShutdownTimeout)
}

// endWithin is end with limit on the export. Only the first call has an
// effect, so an interrupt and the normal exit cannot export twice.
func (t *commandTrace) endWithin(err error, limit time.Duration) {
	if t == nil {
		return
	}
	t.once.Do(func() {
		recordError(t.span, err)
		t.span.End()
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		defer cancel()
		if t.exporter != nil {
			if err := t.exporter.ExportSpans(ctx, t.spans.take()); err != nil {
				t.warnExport(err)
			}
			_ = t.exporter.Shutdown(ctx)
		}
		_ = t.tp.Shutdown(ctx)
	})
}

// warnExport reports a failed export, with the collector it was sent to.
func (t *commandTrace) warnExport(err error) {
	if t.endpoint == "" {
		fmt.Fprintf(t.stderr, "Warning: could not export the command trace: %v\n", err)
		return
	}
	fmt.Fprintf(t.stderr, "Warning: could not export the command trace to %s: %v\n", t.endpoint, err)
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
