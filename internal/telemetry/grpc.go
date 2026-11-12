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
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/stats"
)

// RPCDurationBuckets are the histogram boundaries, in seconds, of
// rpc.server.call.duration and rpc.client.call.duration. otelgrpc's own
// boundaries stop at 10 s, but a resume that restores a snapshot or waits for
// workspace readiness can run for tens of seconds.
var RPCDurationBuckets = []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 15, 60}

// rpcDurationViews applies RPCDurationBuckets to the RPC duration histograms.
func rpcDurationViews() []sdkmetric.View {
	var views []sdkmetric.View
	for _, name := range []string{"rpc.server.call.duration", "rpc.client.call.duration"} {
		views = append(views, sdkmetric.NewView(
			sdkmetric.Instrument{Name: name},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{Boundaries: RPCDurationBuckets}},
		))
	}
	return views
}

// ServerStatsHandler traces and measures the RPCs a gRPC server serves, with
// the global tracer and meter providers. A caller's traceparent becomes the
// parent of the server span; baggage is never read.
//
// Only methods of services are recorded. otelgrpc puts the method name a
// client sends into the span name and rpc.method, so recording unknown names
// would let any client create spans and metric series at will. Calls to other
// methods still propagate trace context but produce no span or measurement.
func ServerStatsHandler(services ...*grpc.ServiceDesc) stats.Handler {
	known := make(map[string]bool)
	for _, sd := range services {
		for _, m := range sd.Methods {
			known["/"+sd.ServiceName+"/"+m.MethodName] = true
		}
		for _, s := range sd.Streams {
			known["/"+sd.ServiceName+"/"+s.StreamName] = true
		}
	}
	return otelgrpc.NewServerHandler(
		otelgrpc.WithPropagators(propagation.TraceContext{}),
		otelgrpc.WithFilter(func(info *stats.RPCTagInfo) bool { return known[info.FullMethodName] }),
	)
}

// ClientStatsHandler traces and measures the RPCs a gRPC client makes, with the
// global tracer and meter providers. Each call is a client span under the span
// in its context and sends that trace context as traceparent, never baggage.
func ClientStatsHandler() stats.Handler {
	return otelgrpc.NewClientHandler(otelgrpc.WithPropagators(propagation.TraceContext{}))
}
