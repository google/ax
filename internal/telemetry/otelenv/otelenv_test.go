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

package otelenv

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

func TestResolveExporters(t *testing.T) {
	known := []string{ExporterOTLP, ExporterConsole}
	def := Exporters{ExporterOTLP: true}
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
		got, err := ResolveExporters("OTEL_TEST_EXPORTER", tt.val, known, def)
		if got.String() != tt.want || (err != nil) != tt.wantErr {
			t.Errorf("ResolveExporters(%q) = %s, %v; want %s, error %v", tt.val, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestResolveExportersDoesNotShareDefault(t *testing.T) {
	def := Exporters{ExporterOTLP: true}
	got, _ := ResolveExporters("OTEL_TEST_EXPORTER", "", []string{ExporterOTLP}, def)
	got[ExporterConsole] = true
	if def.Has(ExporterConsole) {
		t.Error("changing the result changed the default")
	}
}

func attrValue(res *resource.Resource, key attribute.Key) string {
	v, _ := res.Set().Value(key)
	return v.String()
}

func TestResource(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	res, err := Resource(context.Background(), "ax", semconv.ServiceInstanceID("host-1"))
	if err != nil {
		t.Fatalf("Resource: %v", err)
	}
	for key, want := range map[attribute.Key]string{
		semconv.ServiceNameKey:          "ax",
		semconv.ServiceInstanceIDKey:    "host-1",
		semconv.TelemetrySDKLanguageKey: "go",
	} {
		if got := attrValue(res, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if res.SchemaURL() != semconv.SchemaURL {
		t.Errorf("schema URL = %q, want %q", res.SchemaURL(), semconv.SchemaURL)
	}
}

func TestResourceEnvironmentOverrides(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "ax-dev")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.instance.id=pod-uid,deployment.environment.name=test")
	res, err := Resource(context.Background(), "ax", semconv.ServiceInstanceID("host-1"))
	if err != nil {
		t.Fatalf("Resource: %v", err)
	}
	for key, want := range map[attribute.Key]string{
		semconv.ServiceNameKey:        "ax-dev",
		semconv.ServiceInstanceIDKey:  "pod-uid",
		"deployment.environment.name": "test",
	} {
		if got := attrValue(res, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestResourcePartialIsUsable(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "not-a-pair")
	res, err := Resource(context.Background(), "ax")
	if !errors.Is(err, resource.ErrPartialResource) {
		t.Fatalf("err = %v, want a partial resource error", err)
	}
	if res == nil || attrValue(res, semconv.ServiceNameKey) != "ax" {
		t.Errorf("partial resource = %v, want one with service.name=ax", res)
	}
}
