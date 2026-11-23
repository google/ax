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

package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/ax/internal/axattr"
	"github.com/google/ax/internal/telemetry"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"go.opentelemetry.io/otel/log"
)

// TaskStateChanged is the event emitted after a task write that changes the
// task's phase. docs/observability.md describes its attributes.
var TaskStateChanged = telemetry.Event{
	Name:     "ax.task.state_changed",
	Body:     "task state changed",
	Severity: log.SeverityInfo,
}

// Task phases that are not a stored TaskState label.
const (
	// PhaseProvisioned is a task whose actor owns its live state (Running,
	// Suspended, Crashed, ...). Agent Substrate reports those states in
	// ate.actor.state_changed for the same ate.actor.uid.
	PhaseProvisioned = "Provisioned"
	// PhaseDeleted is a task whose record was deleted.
	PhaseDeleted = "Deleted"
)

// taskPhase labels a stored task: Terminating, Queued (the one state that
// the store holds), or Provisioned. A nil task has the phase "", which is no
// record.
func taskPhase(task *v1alpha1.Task) string {
	switch {
	case task == nil:
		return ""
	case task.GetMetadata().GetDeleteTime() != nil:
		return v1alpha1.ReasonTerminating
	case v1alpha1.IsAXTaskState(task.GetStatus().GetState()):
		return v1alpha1.FormatTaskState(task)
	default:
		return PhaseProvisioned
	}
}

// taskWrite is a successful write of a task record: the task's phase before
// and after the write, and the time at which the write finished. The write
// helpers return it, and the caller calls emit, so that the event can carry
// an actor UID that the caller reads after the write.
type taskWrite struct {
	atespace, name, id string
	// previous is "" if the write created the record.
	previous, phase string
	at              time.Time
}

func newTaskWrite(task *v1alpha1.Task, previous, phase string) taskWrite {
	atespace, name := taskKey(task)
	return taskWrite{
		atespace: atespace,
		name:     name,
		id:       task.GetMetadata().GetUid(),
		previous: previous,
		phase:    phase,
		at:       time.Now(),
	}
}

// emit emits TaskStateChanged if the write changed the task's phase.
// actorUID is the task's actor, or "" if the caller did not read it. The
// caller holds the task's lock, so a task's events are in write order.
func (w taskWrite) emit(ctx context.Context, actorUID string) {
	if w.phase == w.previous {
		return
	}
	attrs := []slog.Attr{
		slog.String(string(axattr.TaskName), w.name),
		slog.String(string(axattr.Atespace), w.atespace),
		slog.String(string(axattr.TaskPhase), w.phase),
	}
	if w.previous != "" {
		attrs = append(attrs, slog.String(string(axattr.TaskPreviousPhase), w.previous))
	}
	if w.id != "" {
		attrs = append(attrs, slog.String(string(axattr.TaskID), w.id))
	}
	if actorUID != "" {
		attrs = append(attrs, slog.String(string(axattr.ActorUID), actorUID))
	}
	telemetry.EmitAt(ctx, TaskStateChanged, w.at, attrs...)
}

// taskKey returns the atespace and name of task, with the default atespace if
// it has none.
func taskKey(task *v1alpha1.Task) (atespace, name string) {
	return atespaceOrDefault(task.GetMetadata().GetAtespace()), task.GetMetadata().GetName()
}
