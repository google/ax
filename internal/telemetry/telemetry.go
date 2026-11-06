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

// Package telemetry sets up OpenTelemetry for ax-server: a tracer provider, a
// meter provider, and a logger provider for structured events, all exporting
// over OTLP/gRPC; a Prometheus handler for the same metrics; and a slog
// handler that stamps log lines with the current trace context.
//
// Configuration follows the OpenTelemetry environment variables:
// OTEL_EXPORTER_OTLP_ENDPOINT (and the per-signal variants) choose the
// collector; OTEL_TRACES_EXPORTER, OTEL_METRICS_EXPORTER, and
// OTEL_LOGS_EXPORTER choose the exporters; OTEL_TRACES_SAMPLER and
// OTEL_TRACES_SAMPLER_ARG override the default sampler; and OTEL_SERVICE_NAME
// and OTEL_RESOURCE_ATTRIBUTES override the resource. A value ax-server does
// not understand is logged and ignored, which keeps ax-server's default.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/log/global"
	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// DefaultSampleRatio is the share of new traces sampled unless
// OTEL_TRACES_SAMPLER validly says otherwise. The default sampler is parent
// based, so a caller's sampling decision, carried in traceparent, is followed.
const DefaultSampleRatio = 0.1

// Providers holds the providers Init installed as the OpenTelemetry globals.
type Providers struct {
	Tracer *sdktrace.TracerProvider
	Meter  *sdkmetric.MeterProvider
	// Logger is nil when OTEL_LOGS_EXPORTER does not select otlp. Emit then
	// writes events to stdout.
	Logger *sdklog.LoggerProvider

	registry *prometheus.Registry
}

// Init builds the providers for serviceName, installs them as the
// OpenTelemetry globals with the W3C trace context propagator, and routes SDK
// errors to slog. Call Shutdown on the result before the process exits.
//
// Metrics are pushed every OTEL_METRIC_EXPORT_INTERVAL (60 s by default) while
// OTEL_METRICS_EXPORTER selects otlp, so without a reachable collector the SDK
// logs an export error at that interval. Traces and events are exported only
// once something records them.
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
	if p.Meter, p.registry, err = newMeterProvider(ctx, res); err != nil {
		return nil, errors.Join(err, p.Shutdown(ctx))
	}
	if p.Logger, err = newLoggerProvider(ctx, res); err != nil {
		return nil, errors.Join(err, p.Shutdown(ctx))
	}

	otel.SetTracerProvider(p.Tracer)
	otel.SetMeterProvider(p.Meter)
	if p.Logger != nil {
		global.SetLoggerProvider(p.Logger)
	} else {
		// Emit checks the global provider; a no-op one sends events to stdout.
		global.SetLoggerProvider(lognoop.NewLoggerProvider())
	}
	// Only trace context crosses process boundaries. Baggage from a caller is
	// not trusted, so it is not propagated.
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return p, nil
}

// MetricsHandler serves the meter provider's metrics, together with Go
// runtime and process metrics, in the Prometheus exposition format. The Go
// and process metrics are served here only, not pushed over OTLP.
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
// OTEL_RESOURCE_ATTRIBUTES override the defaults; the bundled manifests set
// service.instance.id to the pod UID and add the k8s.* attributes there.
func newResource(ctx context.Context, serviceName string) (*resource.Resource, error) {
	attrs := resource.WithAttributes(semconv.ServiceName(serviceName))
	opts := []resource.Option{resource.WithTelemetrySDK(), resource.WithSchemaURL(semconv.SchemaURL), attrs}
	// Without OTEL_RESOURCE_ATTRIBUTES, the hostname tells replicas apart.
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
	def := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(DefaultSampleRatio))
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res), sdktrace.WithSampler(resolveSampler(ctx, def))}
	export := resolveExportersEnv(ctx, tracesExporterEnv, []string{exporterOTLP}, exporters{exporterOTLP: true}).has(exporterOTLP)
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

// newMeterProvider builds a meter provider with a Prometheus reader, which
// MetricsHandler serves, and an OTLP periodic reader while
// OTEL_METRICS_EXPORTER selects otlp. The variable controls only the push:
// prometheus or none turns it off, and --metrics-addr controls /metrics.
//
// Each instrument keeps the SDK's default of 2000 attribute sets per
// collection, which OTEL_GO_X_CARDINALITY_LIMIT overrides.
func newMeterProvider(ctx context.Context, res *resource.Resource) (*sdkmetric.MeterProvider, *prometheus.Registry, error) {
	reg := prometheus.NewRegistry()
	if err := reg.Register(collectors.NewGoCollector()); err != nil {
		return nil, nil, fmt.Errorf("registering Go metrics: %w", err)
	}
	if err := reg.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})); err != nil {
		return nil, nil, fmt.Errorf("registering process metrics: %w", err)
	}
	promReader, err := promexporter.New(promexporter.WithRegisterer(reg))
	if err != nil {
		return nil, nil, fmt.Errorf("creating Prometheus exporter: %w", err)
	}
	opts := []sdkmetric.Option{sdkmetric.WithResource(res), sdkmetric.WithReader(promReader)}

	selected := resolveExportersEnv(ctx, metricsExporterEnv, []string{exporterOTLP, exporterPrometheus},
		exporters{exporterOTLP: true, exporterPrometheus: true})
	push := selected.has(exporterOTLP)
	if push {
		exp, err := otlpmetricgrpc.New(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("creating OTLP metric exporter: %w", err)
		}
		opts = append(opts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)))
	}
	mp := sdkmetric.NewMeterProvider(opts...)
	slog.InfoContext(ctx, "metrics initialized", "export", push)
	return mp, reg, nil
}

// newLoggerProvider returns the provider for structured events, or nil when
// OTEL_LOGS_EXPORTER does not select otlp. It also sets whether Emit writes
// events to stdout beside OTLP (console). ax-server's own log lines go to
// stdout, not through this provider.
//
// The default is none rather than the specification's otlp, as in Agent
// Substrate: events stay on stdout, where they are never lost, until an
// operator points them at a collector.
func newLoggerProvider(ctx context.Context, res *resource.Resource) (*sdklog.LoggerProvider, error) {
	selected := resolveExportersEnv(ctx, logsExporterEnv, []string{exporterOTLP, exporterConsole}, exporters{})
	console.Store(selected.has(exporterConsole))
	if !selected.has(exporterOTLP) {
		slog.InfoContext(ctx, "OTLP logs export disabled", "exporter", selected.String())
		return nil, nil
	}
	exp, err := otlploggrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating OTLP log exporter: %w", err)
	}
	// Batched, so an unreachable collector never blocks the caller emitting.
	lp := sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)))
	slog.InfoContext(ctx, "OTLP logs export initialized", "exporter", selected.String())
	return lp, nil
}
