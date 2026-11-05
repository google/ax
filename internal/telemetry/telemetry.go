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

// Package telemetry sets up OpenTelemetry for ax-server: a tracer provider and
// a logger provider that export over OTLP/gRPC, a meter provider read by a
// Prometheus exporter, and a slog handler that stamps log lines with the
// current trace and span IDs.
//
// Configuration follows the OpenTelemetry environment variables:
// OTEL_EXPORTER_OTLP_ENDPOINT (and the per-signal variants) choose the
// collector, OTEL_TRACES_EXPORTER and OTEL_LOGS_EXPORTER choose otlp (the
// default) or none, OTEL_TRACES_SAMPLER overrides the default sampler, and
// OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES override the resource.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

const (
	// CardinalityLimit caps the attribute sets an instrument keeps per
	// collection. Sets beyond it are folded into one overflow series, so a
	// label that turns out to be unbounded cannot exhaust ax-server's memory or
	// the metrics backend.
	CardinalityLimit = 100
	// DefaultSampleRatio is the share of new traces sampled when
	// OTEL_TRACES_SAMPLER is unset. A caller's sampling decision, carried in
	// traceparent, is always followed.
	DefaultSampleRatio = 0.1

	exporterOTLP = "otlp"
	exporterNone = "none"
)

// Providers holds the providers Init installed as the OpenTelemetry globals.
type Providers struct {
	Tracer *sdktrace.TracerProvider
	Meter  *sdkmetric.MeterProvider
	// Logger is nil when OTEL_LOGS_EXPORTER turns OTLP logs off.
	Logger *sdklog.LoggerProvider

	registry *prometheus.Registry
}

// Init builds the providers for serviceName, installs them as the
// OpenTelemetry globals with the W3C trace context propagator, and routes SDK
// errors to slog. Call Shutdown on the result before the process exits.
//
// Nothing is exported until something records a span or emits a log record,
// so a deployment without a collector only sees errors once it does.
func Init(ctx context.Context, serviceName string) (*Providers, error) {
	// The SDK's default handler writes to stderr, outside the JSON logs.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		slog.Warn("OpenTelemetry SDK error", "error", err)
	}))

	res, err := newResource(ctx, serviceName)
	if err != nil {
		return nil, err
	}
	p := &Providers{}
	if p.Tracer, err = newTracerProvider(ctx, res); err != nil {
		return nil, err
	}
	if p.Meter, p.registry, err = newMeterProvider(res); err != nil {
		return nil, errors.Join(err, p.Shutdown(ctx))
	}
	if p.Logger, err = newLoggerProvider(ctx, res); err != nil {
		return nil, errors.Join(err, p.Shutdown(ctx))
	}

	otel.SetTracerProvider(p.Tracer)
	otel.SetMeterProvider(p.Meter)
	if p.Logger != nil {
		global.SetLoggerProvider(p.Logger)
	}
	// Only trace context crosses process boundaries. Baggage from a caller is
	// not trusted, so it is not propagated.
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return p, nil
}

// MetricsHandler serves the meter provider's metrics, together with Go
// runtime and process metrics, in the Prometheus exposition format.
func (p *Providers) MetricsHandler() http.Handler {
	return promhttp.HandlerFor(p.registry, promhttp.HandlerOpts{})
}

// Shutdown flushes and stops the providers. It is safe to call on providers
// that Init only partly built.
func (p *Providers) Shutdown(ctx context.Context) error {
	var errs []error
	if p.Tracer != nil {
		errs = append(errs, p.Tracer.Shutdown(ctx))
	}
	if p.Meter != nil {
		errs = append(errs, p.Meter.Shutdown(ctx))
	}
	if p.Logger != nil {
		errs = append(errs, p.Logger.Shutdown(ctx))
	}
	return errors.Join(errs...)
}

// newResource describes the process. OTEL_SERVICE_NAME and
// OTEL_RESOURCE_ATTRIBUTES override the defaults.
func newResource(ctx context.Context, serviceName string) (*resource.Resource, error) {
	attrs := resource.WithAttributes(semconv.ServiceName(serviceName))
	opts := []resource.Option{resource.WithTelemetrySDK(), resource.WithSchemaURL(semconv.SchemaURL), attrs}
	// In Kubernetes the hostname is the pod name, which tells replicas apart.
	if host, err := os.Hostname(); err == nil {
		opts = append(opts, resource.WithAttributes(semconv.ServiceInstanceID(host)))
	}
	opts = append(opts, resource.WithFromEnv())
	res, err := resource.New(ctx, opts...)
	if errors.Is(err, resource.ErrPartialResource) || errors.Is(err, resource.ErrSchemaURLConflict) {
		slog.WarnContext(ctx, "partial telemetry resource", "error", err)
		return res, nil
	}
	if err != nil {
		return nil, fmt.Errorf("building telemetry resource: %w", err)
	}
	return res, nil
}

func newTracerProvider(ctx context.Context, res *resource.Resource) (*sdktrace.TracerProvider, error) {
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	// Without WithSampler, the SDK reads OTEL_TRACES_SAMPLER itself.
	if strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER")) == "" {
		opts = append(opts, sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(DefaultSampleRatio))))
	}
	export := exporterSelected(ctx, "OTEL_TRACES_EXPORTER")
	if export {
		exp, err := otlptracegrpc.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("creating OTLP trace exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}
	// Without an exporter the provider still samples and propagates context,
	// so traces through ax-server stay whole.
	tp := sdktrace.NewTracerProvider(opts...)
	slog.InfoContext(ctx, "tracing initialized", "export", export)
	return tp, nil
}

func newMeterProvider(res *resource.Resource) (*sdkmetric.MeterProvider, *prometheus.Registry, error) {
	reg := prometheus.NewRegistry()
	if err := reg.Register(collectors.NewGoCollector()); err != nil {
		return nil, nil, fmt.Errorf("registering Go metrics: %w", err)
	}
	if err := reg.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})); err != nil {
		return nil, nil, fmt.Errorf("registering process metrics: %w", err)
	}
	reader, err := promexporter.New(promexporter.WithRegisterer(reg))
	if err != nil {
		return nil, nil, fmt.Errorf("creating Prometheus exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(reader),
		sdkmetric.WithCardinalityLimit(CardinalityLimit),
	)
	return mp, reg, nil
}

// newLoggerProvider returns the provider for structured events, or nil when
// OTEL_LOGS_EXPORTER turns OTLP off. ax-server's own log lines go to stdout,
// not through it.
func newLoggerProvider(ctx context.Context, res *resource.Resource) (*sdklog.LoggerProvider, error) {
	if !exporterSelected(ctx, "OTEL_LOGS_EXPORTER") {
		slog.InfoContext(ctx, "OTLP logs export disabled")
		return nil, nil
	}
	exp, err := otlploggrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating OTLP log exporter: %w", err)
	}
	// Batched, so an unreachable collector never blocks the caller emitting.
	lp := sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)))
	slog.InfoContext(ctx, "OTLP logs export initialized")
	return lp, nil
}

// exporterSelected reports whether the exporter list in the environment
// variable key selects otlp. Unset selects otlp. ax-server knows only otlp and
// none; other names are logged and ignored.
func exporterSelected(ctx context.Context, key string) bool {
	val := strings.TrimSpace(os.Getenv(key))
	if val == "" {
		return true
	}
	selected := false
	for _, name := range strings.Split(val, ",") {
		switch name = strings.ToLower(strings.TrimSpace(name)); name {
		case exporterOTLP:
			selected = true
		case exporterNone:
			return false
		default:
			slog.WarnContext(ctx, "ignoring unsupported OpenTelemetry exporter", "variable", key, "exporter", name)
		}
	}
	return selected
}
