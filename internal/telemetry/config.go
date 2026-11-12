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
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/google/ax/internal/telemetry/otelenv"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// The environment variables ax-server reads itself rather than leaving to the
// SDK, so that a value it does not understand keeps ax-server's default.
const (
	tracesExporterEnv   = "OTEL_TRACES_EXPORTER"
	metricsExporterEnv  = "OTEL_METRICS_EXPORTER"
	logsExporterEnv     = "OTEL_LOGS_EXPORTER"
	tracesSamplerEnv    = "OTEL_TRACES_SAMPLER"
	tracesSamplerArgEnv = "OTEL_TRACES_SAMPLER_ARG"
)

// resolveExportersEnv reads env and applies otelenv.ResolveExporters, the rules
// the ax CLI uses too. A problem with the value is logged, not returned: a
// telemetry setting never stops startup.
func resolveExportersEnv(ctx context.Context, env string, known []string, def otelenv.Exporters) otelenv.Exporters {
	value := os.Getenv(env)
	got, err := otelenv.ResolveExporters(env, value, known, def)
	if err != nil {
		slog.WarnContext(ctx, "invalid OpenTelemetry exporter setting", "variable", env, "value", value, "using", got.String(), "error", err)
	}
	return got
}

// resolveSampler applies OTEL_TRACES_SAMPLER and OTEL_TRACES_SAMPLER_ARG on top
// of def. ax-server reads them itself because the SDK falls back to sampling
// every trace on a value it does not know, and reads a missing or invalid
// ratio as 1.0. Either would multiply the trace volume by ten, so ax-server
// keeps def instead and logs a warning.
func resolveSampler(ctx context.Context, def sdktrace.Sampler) sdktrace.Sampler {
	name, nameSet := os.LookupEnv(tracesSamplerEnv)
	arg, argSet := os.LookupEnv(tracesSamplerArgEnv)
	s, err := parseSampler(name, nameSet, arg, argSet, def)
	if err != nil {
		slog.WarnContext(ctx, "invalid trace sampler setting, keeping the default", "sampler", name, "arg", arg, "default", def.Description(), "error", err)
	}
	return s
}

// parseSampler accepts the sampler names the OpenTelemetry Go SDK knows. Any
// error means def was kept.
func parseSampler(name string, nameSet bool, arg string, argSet bool, def sdktrace.Sampler) (sdktrace.Sampler, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	// Unlike the SDK, an empty value reads as unset.
	if !nameSet || name == "" {
		return def, nil
	}
	var ratio float64
	if name == "traceidratio" || name == "parentbased_traceidratio" {
		trimmed := strings.TrimSpace(arg)
		if !argSet || trimmed == "" {
			return def, fmt.Errorf("%s %q requires %s to be a ratio in [0, 1]", tracesSamplerEnv, name, tracesSamplerArgEnv)
		}
		parsed, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return def, fmt.Errorf("parsing %s %q: %w", tracesSamplerArgEnv, arg, err)
		}
		if parsed < 0 || parsed > 1 {
			return def, fmt.Errorf("%s %q is outside [0, 1]", tracesSamplerArgEnv, arg)
		}
		ratio = parsed
	}
	switch name {
	case "always_on":
		return sdktrace.AlwaysSample(), nil
	case "always_off":
		return sdktrace.NeverSample(), nil
	case "traceidratio":
		return sdktrace.TraceIDRatioBased(ratio), nil
	case "parentbased_always_on":
		return sdktrace.ParentBased(sdktrace.AlwaysSample()), nil
	case "parentbased_always_off":
		return sdktrace.ParentBased(sdktrace.NeverSample()), nil
	case "parentbased_traceidratio":
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio)), nil
	}
	return def, fmt.Errorf("unsupported %s %q", tracesSamplerEnv, name)
}
