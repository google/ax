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
	"fmt"
	"os"

	"cloud.google.com/go/compute/metadata"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	oauth2google "golang.org/x/oauth2/google"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/google"
)

const defaultEndpoint = "telemetry.googleapis.com"

// WithGCPCredentials returns an option that configures the OTLP exporter to use Google Cloud credentials.
func WithGCPCredentials() otlptracegrpc.Option {
	bundle := google.NewDefaultCredentials()
	return otlptracegrpc.WithDialOption(
		grpc.WithTransportCredentials(bundle.TransportCredentials()),
		grpc.WithPerRPCCredentials(bundle.PerRPCCredentials()),
	)
}

// SetTraceProvider initializes the OpenTelemetry SDK.
// It returns a shutdown function that should be called when the application exits.
func SetTraceProvider(ctx context.Context, service string, opts ...otlptracegrpc.Option) (func(context.Context) error, error) {
	// 1. Set global propagator. This is crucial for context propagation over gRPC/HTTP.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// 2. Create OTLP Exporter.
	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP trace exporter: %w", err)
	}

	// 3. Define Resource.
	attrs := []attribute.KeyValue{
		semconv.ServiceNameKey.String(service),
	}
	if projectID := detectGCPProjectID(ctx); projectID != "" {
		attrs = append(attrs, attribute.String("gcp.project_id", projectID))
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(attrs...),
		resource.WithProcess(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	// 4. Create TracerProvider.
	bsp := sdktrace.NewBatchSpanProcessor(exporter)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(bsp),
	)

	otel.SetTracerProvider(tp)

	return func(shutdownCtx context.Context) error {
		return tp.Shutdown(shutdownCtx)
	}, nil
}

func detectGCPProjectID(ctx context.Context) string {
	if proj := os.Getenv("GOOGLE_CLOUD_PROJECT"); proj != "" {
		return proj
	}
	if metadata.OnGCEWithContext(ctx) {
		if proj, err := metadata.ProjectIDWithContext(ctx); err == nil {
			return proj
		}
	}
	if creds, err := oauth2google.FindDefaultCredentials(ctx); err == nil && creds.ProjectID != "" {
		return creds.ProjectID
	}
	return ""
}
