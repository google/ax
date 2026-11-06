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

## Attribute names

Spans, events, and log attributes use the names in
[`internal/axattr`](../internal/axattr/axattr.go). Attributes that identify an
actor reuse Agent Substrate's names, `ate.atespace` and `ate.actor.uid`, so
ax-server and Substrate records join on them. Identity attributes such as
`ax.task.id`, `ate.atespace`, and `ate.actor.uid` never appear on metrics.
