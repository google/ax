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

// Package axattr names the OpenTelemetry attributes AX puts on its spans, logs,
// and events, so every emitter spells them the same way. Attributes that
// identify an actor reuse Agent Substrate's names, so AX and Substrate records
// join on them.
//
// Keep these attributes off metrics:
//   - The identity attributes (task ID and name, atespace, actor UID) have one
//     value per task or per user-chosen atespace, so their set of values is
//     unbounded. Agent Substrate's no-actor-identity rule also keeps ate.atespace
//     and ate.actor.uid off its metrics.
//   - The exit code and the signal have a bounded set of values, but they
//     describe one task's command, so ax-server records them only beside the
//     task identity, on spans, logs, and events.
package axattr

import "go.opentelemetry.io/otel/attribute"

const (
	// TaskID is the task's status.id, unique across the task's lifetime.
	TaskID = attribute.Key("ax.task.id")
	// TaskName is the task's metadata.name.
	TaskName = attribute.Key("ax.task.name")
	// TaskPhase is the task's status.phase, for example Running.
	TaskPhase = attribute.Key("ax.task.phase")
	// TaskResidency is where the task's actor is in its lifecycle: queued,
	// resuming, running, suspending, suspended, reverting, crashed, deleting,
	// or deleted.
	TaskResidency = attribute.Key("ax.task.residency")
	// TaskOutcome is how the task's command ended: succeeded, failed, or
	// cancelled.
	TaskOutcome = attribute.Key("ax.task.outcome")
	// TaskOutcomeSource is what decided the outcome: sandbox, orchestrator, or
	// external.
	TaskOutcomeSource = attribute.Key("ax.task.outcome_source")
	// TaskExitCode is the exit code of the task's command.
	TaskExitCode = attribute.Key("ax.task.exit_code")
	// TaskSignal is the signal that ended the task's command.
	TaskSignal = attribute.Key("ax.task.signal")

	// Atespace is the atespace of the task and its actor, named as Substrate
	// names it.
	Atespace = attribute.Key("ate.atespace")
	// ActorUID is the UID of the task's actor, named as Substrate names it.
	ActorUID = attribute.Key("ate.actor.uid")
)
