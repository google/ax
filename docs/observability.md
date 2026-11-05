# Observability

ax-server sets up OpenTelemetry itself. It needs no sidecar, but it does need
a collector to send traces and events to.

| Signal | Where it goes | Default |
|---|---|---|
| Logs | stdout, one JSON object per line | Always on |
| Metrics | Prometheus `/metrics` on port 9090 (`--metrics-addr`) | On; `--metrics-addr=` turns it off |
| Traces | OTLP/gRPC to a collector | Exported; 10% of new traces sampled |
| Events | OTLP/gRPC log records to a collector | Exported |

Health probes and the AX API stay on port 8080. The bundled manifests expose
port 9090 as `metrics` on the `ax-server` Service, so a Prometheus scrape
config or a GKE `PodMonitoring` can target it.

## Configuration

Telemetry is configured with the standard
[OpenTelemetry environment variables](https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/)
on the ax-server container:

| Variable | Effect |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Collector address, for example `http://otel-collector.observability.svc:4317`. The SDK default is `localhost:4317`. An `http://` endpoint is reached without TLS. |
| `OTEL_TRACES_EXPORTER`, `OTEL_LOGS_EXPORTER` | `otlp` (default) or `none`. With `none`, traces are still sampled and propagated, only not sent. |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | Replace the default sampler, `parentbased_traceidratio` at `0.1`. A caller's sampling decision is always followed. |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | Override the resource. The defaults are `service.name=ax-server` and `service.instance.id` set to the pod name. |

Nothing is sent until ax-server records a span or emits an event, so a
deployment without a collector logs no export errors until then.

## Logs

ax-server writes structured JSON logs to stdout. A line logged while a trace
is active carries `trace_id` and `span_id`, so a log backend can link it to
the trace. ax-server's log lines are not sent over OTLP; collect them from
stdout as you would for any pod. OTLP log records carry structured events
only.

## Metrics

Each instrument keeps at most 100 attribute sets per collection. Further sets
are folded into one series labelled `otel_metric_overflow="true"`, so an
unbounded label cannot exhaust memory. `/metrics` also serves the Go runtime
and process metrics (`go_*`, `process_*`).

## Attribute names

Spans, events, and log attributes use the names in
[`internal/axattr`](../internal/axattr/axattr.go). Attributes that identify an
actor reuse Agent Substrate's names, `ate.atespace` and `ate.actor.uid`, so
ax-server and Substrate records join on them. Identity attributes such as
`ax.task.id` and `ate.actor.uid` never appear on metrics.
