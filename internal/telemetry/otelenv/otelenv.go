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

// Package otelenv holds the OpenTelemetry configuration rules that ax-server
// and the ax CLI share: how an OTEL_*_EXPORTER variable is read, and how a
// process describes itself as a resource. It depends only on the OpenTelemetry
// API and SDK, so the CLI can use it without linking ax-server's exporters.
package otelenv

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// The names the OTEL_*_EXPORTER variables use. None selects the empty set; it
// is not an exporter.
const (
	ExporterOTLP       = "otlp"
	ExporterConsole    = "console"
	ExporterPrometheus = "prometheus"
	ExporterNone       = "none"
)

// Exporters is the set of exporters an OTEL_*_EXPORTER variable selects. The
// empty set is none.
type Exporters map[string]bool

// Has reports whether name is selected.
func (e Exporters) Has(name string) bool { return e[name] }

// String lists the names in order, or none.
func (e Exporters) String() string {
	if len(e) == 0 {
		return ExporterNone
	}
	return strings.Join(slices.Sorted(maps.Keys(e)), ",")
}

// ResolveExporters reads value, the value of the variable env, as a
// comma-separated exporter list, as the OpenTelemetry specification allows.
// Names are trimmed and lowercased; empty and unknown names are skipped with
// an error, and the rest apply. A value that is empty, names no known
// exporter, or lists none with an exporter keeps def, as the specification
// asks for a setting it cannot use. The returned set is the one to use even
// when err is not nil; the caller reports err as a warning.
func ResolveExporters(env, value string, known []string, def Exporters) (Exporters, error) {
	// Unset and empty read the same, because templated manifests can render
	// an empty variable.
	if strings.TrimSpace(value) == "" {
		return maps.Clone(def), nil
	}
	got := Exporters{}
	none := false
	var skipped []string
	for item := range strings.SplitSeq(value, ",") {
		switch name := strings.ToLower(strings.TrimSpace(item)); {
		case name == ExporterNone:
			none = true
		case slices.Contains(known, name):
			got[name] = true
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
		return maps.Clone(def), fmt.Errorf("%s %q lists none with an exporter", env, value)
	case none:
		return Exporters{}, skipErr
	case len(got) == 0:
		return maps.Clone(def), fmt.Errorf("%s %q names no known exporter", env, value)
	}
	return got, skipErr
}

// Resource describes a process as serviceName with the SDK attributes and
// attrs. OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES override them.
//
// A partial resource or a schema URL conflict still gives a usable resource,
// returned together with the error to warn about. Any other error returns a
// nil resource.
func Resource(ctx context.Context, serviceName string, attrs ...attribute.KeyValue) (*resource.Resource, error) {
	res, err := resource.New(ctx,
		resource.WithTelemetrySDK(),
		resource.WithSchemaURL(semconv.SchemaURL),
		resource.WithAttributes(append([]attribute.KeyValue{semconv.ServiceName(serviceName)}, attrs...)...),
		resource.WithFromEnv(),
	)
	switch {
	case err == nil:
		return res, nil
	case errors.Is(err, resource.ErrPartialResource), errors.Is(err, resource.ErrSchemaURLConflict):
		return res, err
	default:
		return nil, err
	}
}
