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
	"github.com/google/ax/internal/telemetry"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
)

const defaultEndpoint = "telemetry.googleapis.com"

// gcpTelemetryOpts returns the OTLP options for Google Cloud Trace if the endpoint
// matches the Google Cloud Trace endpoint. It returns the options and a boolean
// indicating if the endpoint was matched.
func gcpTelemetryOpts(endpoint string) ([]otlptracegrpc.Option, bool) {
	if endpoint == defaultEndpoint || endpoint == defaultEndpoint+":443" {
		return []otlptracegrpc.Option{telemetry.WithGCPCredentials()}, true
	}
	return nil, false
}
