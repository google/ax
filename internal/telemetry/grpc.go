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
	"sync"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/stats"
)

// rpcDurationBuckets are the histogram boundaries, in seconds, of
// rpc.server.call.duration and rpc.client.call.duration. The otelgrpc
// boundaries stop at 10 s. A resume can wait for a snapshot restore and for
// the 15 s workspace readiness timeout, so the boundaries go to 60 s, with
// boundaries on each side of 15 s.
var rpcDurationBuckets = []float64{0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 15, 20, 30, 60}

// rpcDurationViews applies rpcDurationBuckets to the RPC duration histograms.
// The client view also removes server.address and server.port. otelgrpc sets
// them from the peer IP address, which adds one series for each Substrate
// replica that answers.
func rpcDurationViews() []sdkmetric.View {
	buckets := sdkmetric.AggregationExplicitBucketHistogram{Boundaries: rpcDurationBuckets}
	return []sdkmetric.View{
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: "rpc.server.call.duration"},
			sdkmetric.Stream{Aggregation: buckets},
		),
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: "rpc.client.call.duration"},
			sdkmetric.Stream{
				Aggregation:     buckets,
				AttributeFilter: attribute.NewDenyKeysFilter("server.address", "server.port"),
			},
		),
	}
}

// ServerStatsHandler traces and measures the RPCs that a gRPC server serves.
// It uses the global tracer and meter providers. The server span is a child of
// the caller's traceparent. The handler does not read baggage.
//
// services returns the services of the server. The handler records only the
// methods of these services. otelgrpc copies the method name from the client
// into the span name and into rpc.method. Thus, if the handler recorded
// unknown names, a client could make new span names and metric series. A call
// to another method still propagates the trace context, but it makes no span
// and no measurement. The handler reads services on the first RPC, after the
// server has registered all its services.
func ServerStatsHandler(services func() map[string]grpc.ServiceInfo) stats.Handler {
	known := sync.OnceValue(func() map[string]bool {
		methods := make(map[string]bool)
		for name, info := range services() {
			for _, m := range info.Methods {
				methods["/"+name+"/"+m.Name] = true
			}
		}
		return methods
	})
	return otelgrpc.NewServerHandler(
		otelgrpc.WithPropagators(propagation.TraceContext{}),
		otelgrpc.WithFilter(func(info *stats.RPCTagInfo) bool { return known()[info.FullMethodName] }),
	)
}

// ClientStatsHandler traces and measures the RPCs that a gRPC client makes. It
// uses the global tracer and meter providers. Each call is a client span under
// the span in its context. The handler sends that trace context as
// traceparent. It does not send baggage.
func ClientStatsHandler() stats.Handler {
	return otelgrpc.NewClientHandler(otelgrpc.WithPropagators(propagation.TraceContext{}))
}
