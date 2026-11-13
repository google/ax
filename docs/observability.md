# Observability

ax-server sets up OpenTelemetry itself and needs no sidecar. It exports
traces, metrics, and events over OTLP/gRPC to a collector you choose, and
serves metrics for Prometheus as well.

| Signal | Where it goes | Default in the code | Default in `deploy/ax-server.yaml` |
|---|---|---|---|
| Logs | stdout, one JSON object per line | Always on | Always on |
| Metrics | OTLP push, and Prometheus `/metrics` on port 9090 (`--metrics-addr`) | Both on | `/metrics` only |
| Traces | OTLP push; 10% of new traces sampled | On | Off |
| Events | stdout as JSON log lines, or OTLP log records | stdout | stdout |

Health probes and the AX API stay on port 8080. The bundled manifests expose
port 9090 as `metrics` on the `ax-server` Service, so a Prometheus scrape
config or a GKE `PodMonitoring` can target it. `--metrics-addr=` turns
`/metrics` off.

## Connect a collector

The manifests read every `OTEL_*` setting from the `ax-otel-config` ConfigMap
in `ax-system`. `deploy/ax-server.yaml` ships with OTLP export off, so an
installation without a collector logs no export errors and loses nothing:
metrics stay on `/metrics` and events go to stdout. To export, edit the
ConfigMap and restart ax-server:

```yaml
data:
  OTEL_EXPORTER_OTLP_ENDPOINT: http://otel-collector.observability.svc:4317
  OTEL_LOGS_EXPORTER: otlp        # also send events; add ,console to keep them on stdout
  # delete OTEL_TRACES_EXPORTER: none and OTEL_METRICS_EXPORTER: prometheus
```

```bash
kubectl -n ax-system rollout restart deployment/ax-server
```

On a cluster that runs Agent Substrate, the endpoint can be the one in
`ate-otel-config` in `ate-system`, so ax-server and Substrate telemetry reach
the same backend. The E2E manifests point at the collector that Substrate's
kind overlay deploys.

The manifests also set `OTEL_RESOURCE_ATTRIBUTES` from the downward API:
`k8s.namespace.name`, `k8s.pod.name`, `k8s.pod.uid`, and `service.instance.id`
set to the pod UID, as Substrate does. This variable is in the Deployment's
`env`, which overrides `envFrom`, so the same key in `ax-otel-config` has no
effect. Add other resource attributes to the value in the Deployment.

## Configuration

Telemetry follows the standard
[OpenTelemetry environment variables](https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/).
As the specification asks, ax-server warns about a value it does not
recognize and ignores it, which keeps the default in the table below.

| Variable | Effect |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Collector address, for example `http://otel-collector.observability.svc:4317`. The SDK default is `localhost:4317`. An `http://` endpoint is reached without TLS. |
| `OTEL_TRACES_EXPORTER` | `otlp` (default) or `none`. With `none`, traces are still sampled and propagated, only not sent. |
| `OTEL_METRICS_EXPORTER` | Controls only the OTLP push: `otlp` (default) turns it on; `prometheus` or `none` turns it off. `/metrics` is controlled by `--metrics-addr`. |
| `OTEL_LOGS_EXPORTER` | Where events go: `none` (default) writes them to stdout; `otlp` sends them to the collector instead; `otlp,console` does both. |
| `OTEL_METRIC_EXPORT_INTERVAL` | Milliseconds between metric pushes; the SDK default is 60000. |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | Replace the default sampler, `parentbased_traceidratio` at `0.1`. With a `parentbased_*` sampler, ax-server always follows the sampling decision of the caller. An unknown sampler, or a ratio sampler whose argument is missing or outside [0, 1], keeps the default. |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | Override the resource. The code defaults are `service.name=ax-server` and `service.instance.id` set to the hostname. |
| `OTEL_GO_X_CARDINALITY_LIMIT` | Attribute sets each metric instrument keeps per collection; the SDK default is 2000. |

The OTLP metric push runs every interval whether or not anything recorded a
metric, so with the push on and no reachable collector the SDK logs an export
error each interval. Traces and events are exported only once ax-server
records them.

## Logs

ax-server writes structured JSON logs to stdout. A line logged while a trace
is active carries `trace_id`, `span_id`, and `trace_flags`, the fields the
OpenTelemetry specification defines for trace context in non-OTLP logs, so a
log backend can link it to the trace. ax-server's log lines are not sent over
OTLP; collect them from stdout as you would for any pod.

## Events

Structured events, such as task state changes, use the OTLP logs signal:
each is a log record with an event name. With `OTEL_LOGS_EXPORTER` unset or
`none`, an event is written to stdout as a JSON log line instead, with the
event name in `event`, so it is never dropped because OTLP is off. This
follows Agent Substrate, whose actor events default to stdout the same way.

OTLP event records are batched, so a slow collector never delays the task
operation that emits them. Records still queued when ax-server exits
ungracefully are lost, unless `console` also wrote them to stdout.

## Metrics

Each instrument keeps at most 2000 attribute sets per collection (the SDK
default, which `OTEL_GO_X_CARDINALITY_LIMIT` overrides). Further sets are
folded into one series labelled `otel_metric_overflow="true"`, so an
unbounded label cannot exhaust memory. `/metrics` also serves the Go runtime
and process metrics (`go_*`, `process_*`); they are not pushed over OTLP.

ax-server records `rpc.server.call.duration` for the AX RPCs that it serves.
It records `rpc.client.call.duration` for its calls to Substrate. Both
histograms have three labels: `rpc.system.name`, `rpc.method`, and
`rpc.response.status_code`. ax-server removes the `server.address` and
`server.port` labels that otelgrpc adds to the client histogram, because
their values are peer IP addresses.

The buckets are 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 15, 20, 30, and 60
seconds. A resume can wait for a snapshot restore and for the 15-second
workspace readiness timeout. Thus the buckets go to 60 seconds, with
boundaries on each side of 15 seconds.

`OTEL_SEMCONV_STABILITY_OPT_IN=rpc/dup` makes otelgrpc also record the old
histograms, `rpc.server.duration` and `rpc.client.duration`, in milliseconds.
These buckets do not apply to them, so their buckets stop at 10 seconds.

## Traces

ax-server records a server span for each AX RPC that it serves. It records a
client span for each call that it makes to Substrate. Reconciles run inside
the RPC, so `CreateActor` and `ResumeActor` show under the `CreateTask` or
`ResumeTask` span.

- **Trace context:** the server span is a child of the caller's
  `traceparent`. ax-server sends `traceparent` to Substrate, which also
  samples ParentBased. ax-server does not read or send `baggage`.
- **Sampling by the caller:** a caller that sends a sampled `traceparent`
  makes ax-server and Substrate record the call, so the 10% ratio does not
  apply to it. `ax --trace` needs this behavior. ax-server has no
  authentication yet, so it trusts this flag from all callers.
- **Unknown methods:** a call to a method that ax-server does not serve
  gets an error. It does not get a span or a metric point. Thus a client
  cannot make new span names or new `rpc.method` values.
- **Errors:** a Substrate call that does not return `OK` has the span status
  `Error`, also when the code is an expected result. For example, a delete
  polls `GetActor` until it returns `NotFound`.

`deploy/ax-server.yaml` sets `OTEL_TRACES_EXPORTER: none`, so ax-server
exports no spans by default. ax-server still sends its span IDs to
Substrate. If Substrate exports its spans, your backend then shows Substrate
spans whose parent spans are missing. To see the full trace, set an OTLP
endpoint and remove that line, as [Connect a collector](#connect-a-collector)
shows.

## CLI traces

`ax --trace <command>` traces one command. The CLI prints the trace ID to
stderr, and every AX RPC of the command sends a sampled W3C `traceparent`.
Each RPC is a client span under the command span, and resolving the
ax-server address, which can start a port-forward, is a child span too.
Without `--trace`, the CLI creates no tracer and sends no trace context, so
ax-server's own sampler decides for CLI traffic.

The CLI exports its spans over OTLP/gRPC only when
`OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is set
in your shell. This is unlike ax-server, which exports by default and uses
the SDK default `localhost:4317` when no endpoint is set. The spans have
`service.name=ax`. `OTEL_TRACES_EXPORTER`, `OTEL_SERVICE_NAME`, and
`OTEL_RESOURCE_ATTRIBUTES` follow the same rules as for ax-server.

The CLI exports once, when the command ends, and waits at most 2 seconds.
It does not retry. If the export fails, the CLI prints a warning that names
the endpoint and gives the error.

ax-server reads `traceparent`. Thus each AX RPC of a `--trace` command is a
server span under the client span of the CLI. If ax-server exports its spans
(see [Traces](#traces)), one trace goes from the CLI through ax-server to
Substrate.

## Attribute names

Spans, events, and log attributes use the names in
[`internal/axattr`](../internal/axattr/axattr.go). Attributes that identify an
actor reuse Agent Substrate's names, `ate.atespace` and `ate.actor.uid`, so
ax-server and Substrate records join on them. Identity attributes such as
`ax.task.id`, `ate.atespace`, and `ate.actor.uid` never appear on metrics.
