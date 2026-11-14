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

import (
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

// NextReconcileAt returns when state next requires background worker attention,
// or nil when the task is at rest and must remain out of the queue.
//
// Rules:
//  1. If pending actions exist, the row is due at max(now, actions[0].NotBefore).
//  2. Otherwise, if the task is in QUEUED (not deleting) with a ClaimDeadline,
//     the row is due at that exact ClaimDeadline (no periodic polling).
//  3. Steady-state tasks (WORKING, IDLE, SUSPENDED, terminal) return nil.
func NextReconcileAt(state State, now time.Time) *time.Time {
	if len(state.Actions) > 0 {
		due := now
		if nb := state.Actions[0].NotBefore; !nb.IsZero() && nb.After(now) {
			due = nb
		}
		return &due
	}
	if state.Task.GetMetadata().GetDeleteTime() == nil &&
		state.Task.GetStatus().GetState() == v1alpha1.TaskState_TASK_STATE_QUEUED &&
		state.ClaimDeadline != nil {
		deadline := *state.ClaimDeadline
		return &deadline
	}
	return nil
}
