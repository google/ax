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

// Package taskevents emits ax.task.state_changed for every committed task row
// write that changes the task's phase, outcome, or actor. It wraps
// [store.TaskApplier], the one path through which ax-server writes task rows,
// so new write sites emit without further changes.
package taskevents

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/ax/internal/axattr"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/internal/telemetry"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"go.opentelemetry.io/otel/log"
)

// StateChanged is the event emitted after a committed task row write that
// changes the task's phase, outcome, or actor.
var StateChanged = telemetry.Event{
	Name:     "ax.task.state_changed",
	Body:     "task state changed",
	Severity: log.SeverityInfo,
}

// Phases that are not a stored TaskState label.
const (
	// PhaseProvisioned is a task whose actor owns its live state (Working,
	// Suspended, Crashed, ...); ate.actor.state_changed for the same
	// ate.actor.uid reports those transitions.
	PhaseProvisioned = "Provisioned"
	// PhaseDeleted is a task whose row was deleted.
	PhaseDeleted = "Deleted"
)

// Wrap returns a [store.TaskApplier] that delegates to next and emits
// [StateChanged] after each committed write that changes the task's phase,
// outcome, or actor. It returns nil if next is nil.
func Wrap(next store.TaskApplier) store.TaskApplier {
	if next == nil {
		return nil
	}
	return &applier{next: next}
}

type applier struct {
	next store.TaskApplier
}

func (a *applier) GetTaskRow(ctx context.Context, key store.TaskKey) (*store.TaskRow, error) {
	return a.next.GetTaskRow(ctx, key)
}

func (a *applier) Apply(ctx context.Context, key store.TaskKey, fn store.ApplyFunc) (*store.TaskRow, error) {
	// fn runs under the row lock. On a retry the last call wins, and it saw
	// the row the commit replaced and the commit's timestamp.
	var (
		before  *snapshot
		at      time.Time
		deleted bool
	)
	written, err := a.next.Apply(ctx, key, func(current *store.TaskRow, now time.Time) (*store.TaskRow, error) {
		before, at = snapshotOf(current), now
		next, err := fn(current, now)
		deleted = err == nil && next == store.DeleteTaskRow
		return next, err
	})
	if err != nil {
		return written, err
	}
	after := snapshotOf(written)
	switch {
	case deleted && before != nil:
		gone := *before
		gone.phase = PhaseDeleted
		gone.generation = before.generation + 1
		after = &gone
	case after == nil:
		return written, nil
	}
	if !changed(before, after) {
		return written, nil
	}
	telemetry.EmitAt(ctx, StateChanged, at, attrs(key.Normalized(), before, after)...)
	return written, nil
}

func changed(before, after *snapshot) bool {
	if before == nil {
		return true
	}
	return before.phase != after.phase ||
		before.outcome != after.outcome ||
		(after.actorUID != "" && before.actorUID != after.actorUID)
}

// snapshot is the part of a task row that ax.task.state_changed reports.
type snapshot struct {
	id         string
	actorUID   string
	phase      string
	outcome    string
	exitCode   int32
	generation int64
}

func snapshotOf(row *store.TaskRow) *snapshot {
	if row == nil || row.Task == nil {
		return nil
	}
	return &snapshot{
		id:         row.Task.GetMetadata().GetUid(),
		actorUID:   row.ActorUID,
		phase:      phase(row.Task),
		outcome:    outcome(row.Task.GetStatus().GetOutcome().GetResult()),
		exitCode:   row.Task.GetStatus().GetOutcome().GetExitCode(),
		generation: row.Generation,
	}
}

// phase labels a stored task. Stored rows hold only AX-owned states, so a
// task whose actor owns its state is stored as UNSPECIFIED.
func phase(task *v1alpha1.Task) string {
	if task.GetMetadata().GetDeleteTime() == nil &&
		task.GetStatus().GetState() == v1alpha1.TaskState_TASK_STATE_UNSPECIFIED {
		return PhaseProvisioned
	}
	return v1alpha1.FormatTaskState(task)
}

func outcome(r v1alpha1.TaskResult) string {
	if r == v1alpha1.TaskResult_TASK_RESULT_UNSPECIFIED {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(r.String(), "TASK_RESULT_"))
}

func attrs(key store.TaskKey, before, after *snapshot) []slog.Attr {
	out := []slog.Attr{
		slog.String(string(axattr.TaskName), key.Name),
		slog.String(string(axattr.Atespace), key.Atespace),
		slog.String(string(axattr.TaskPhase), after.phase),
		slog.Int64(string(axattr.TaskGeneration), after.generation),
	}
	id, actorUID := after.id, after.actorUID
	if before != nil {
		out = append(out, slog.String(string(axattr.TaskPreviousPhase), before.phase))
		if before.outcome != "" {
			out = append(out, slog.String(string(axattr.TaskPreviousOutcome), before.outcome))
		}
		if id == "" {
			id = before.id
		}
		if actorUID == "" {
			actorUID = before.actorUID
		}
	}
	if id != "" {
		out = append(out, slog.String(string(axattr.TaskID), id))
	}
	if actorUID != "" {
		out = append(out, slog.String(string(axattr.ActorUID), actorUID))
	}
	if after.outcome != "" {
		out = append(out, slog.String(string(axattr.TaskOutcome), after.outcome))
		// A cancelled task's command did not exit on its own.
		if after.outcome != "cancelled" {
			out = append(out, slog.Int64(string(axattr.TaskExitCode), int64(after.exitCode)))
		}
	}
	return out
}
