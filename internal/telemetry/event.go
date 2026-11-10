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
	"log/slog"
	"math"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
)

// EventScope is the instrumentation scope of every event ax-server emits.
const EventScope = "github.com/google/ax/internal/telemetry"

// Event is a kind of structured event, such as a task state change. Name is
// the OTLP log record's event name. Body and Severity belong to the kind, not
// the call site, so the OTLP and stdout forms of an event cannot differ.
type Event struct {
	Name     string
	Body     string
	Severity log.Severity
}

// level maps the event's severity onto slog's four levels.
func (ev Event) level() slog.Level {
	switch {
	case ev.Severity >= log.SeverityError:
		return slog.LevelError
	case ev.Severity >= log.SeverityWarn:
		return slog.LevelWarn
	case ev.Severity >= log.SeverityInfo:
		return slog.LevelInfo
	default:
		return slog.LevelDebug
	}
}

// console makes Emit also write the stdout form of an event that went over
// OTLP. Init sets it from OTEL_LOGS_EXPORTER.
var console atomic.Bool

// Emit records ev with attrs. It sends an OTLP log record when
// OTEL_LOGS_EXPORTER selects otlp, and writes the event as a JSON log line
// through slog.Default() when it does not, or also when console is selected.
// An event is therefore never dropped because OTLP is off.
//
// The OTLP record is batched, so a slow or unreachable collector never blocks
// the caller. A record still queued when the process dies is lost, unless
// console also wrote it to stdout.
func Emit(ctx context.Context, ev Event, attrs ...slog.Attr) {
	t := time.Now()
	if emitOTLP(ctx, ev, t, attrs) && !console.Load() {
		return
	}
	h := slog.Default().Handler()
	if !h.Enabled(ctx, ev.level()) {
		return
	}
	rec := slog.NewRecord(t, ev.level(), ev.Body, 0)
	rec.AddAttrs(slog.String("event", ev.Name))
	rec.AddAttrs(attrs...)
	_ = h.Handle(ctx, rec)
}

// emitOTLP sends the OTLP record and reports whether it did. Init installs a
// no-op provider when OTLP logs are off, so the check is cheap.
func emitOTLP(ctx context.Context, ev Event, t time.Time, attrs []slog.Attr) bool {
	logger := global.GetLoggerProvider().Logger(EventScope)
	if !logger.Enabled(ctx, log.EnabledParameters{Severity: ev.Severity, EventName: ev.Name}) {
		return false
	}
	var rec log.Record
	rec.SetEventName(ev.Name)
	rec.SetTimestamp(t)
	rec.SetSeverity(ev.Severity)
	rec.SetBody(attribute.StringValue(ev.Body))
	kvs := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		kvs = append(kvs, attribute.KeyValue{Key: attribute.Key(a.Key), Value: attrValue(a.Value)})
	}
	rec.AddAttributes(kvs...)
	// The SDK takes the trace and span IDs from ctx.
	logger.Emit(ctx, rec)
	return true
}

// attrValue converts an slog value to the attribute kind slog's JSON handler
// writes, so the two forms of an event match.
func attrValue(v slog.Value) attribute.Value {
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return attribute.StringValue(v.String())
	case slog.KindInt64:
		return attribute.Int64Value(v.Int64())
	case slog.KindUint64:
		// OTel has no unsigned kind; clamp rather than wrap.
		if u := v.Uint64(); u <= math.MaxInt64 {
			return attribute.Int64Value(int64(u))
		}
		return attribute.Int64Value(math.MaxInt64)
	case slog.KindFloat64:
		return attribute.Float64Value(v.Float64())
	case slog.KindBool:
		return attribute.BoolValue(v.Bool())
	case slog.KindDuration:
		// Nanoseconds, as slog's JSON handler writes them.
		return attribute.Int64Value(int64(v.Duration()))
	case slog.KindTime:
		return attribute.StringValue(v.Time().Format(time.RFC3339Nano))
	case slog.KindGroup:
		kvs := make([]attribute.KeyValue, 0, len(v.Group()))
		for _, a := range v.Group() {
			kvs = append(kvs, attribute.KeyValue{Key: attribute.Key(a.Key), Value: attrValue(a.Value)})
		}
		return attribute.MapValue(kvs...)
	default:
		return attribute.StringValue(v.String())
	}
}
