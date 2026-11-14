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

package lifecycle

import "time"

// Transition computes the next task state and complete pending action list for
// input in at timestamp now.
//
// Contract invariants for implementations and callers:
//   - Pure & I/O-free: Transition never mutates its input State in place,
//     performs zero I/O, returns the complete replacement State.Actions slice
//     (preserving head Action.ID when an in-flight action must remain valid),
//     and sets Output.DeleteRow = true only when ActionDeleteActor completes.
//   - Level-triggered observations: ObservedActorInput and HeartbeatLostInput
//     are idempotent across repeated sweeps (see their type docs).
//
// TODO(ws1-state-machine): Implement state transitions and runner claim/event
// inputs in the Runner + ax-server workstream.
func Transition(state State, _ Input, _ time.Time) (State, Output, error) {
	return state, Output{}, ErrUnimplemented
}
