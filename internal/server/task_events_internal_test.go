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
	"testing"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestTaskPhase checks the phase of a stored task for every state, with and
// without delete_time.
func TestTaskPhase(t *testing.T) {
	for v := range v1alpha1.TaskState_name {
		state := v1alpha1.TaskState(v)
		want := PhaseProvisioned
		if state == v1alpha1.TaskState_TASK_STATE_QUEUED {
			want = "Queued"
		}
		task := &v1alpha1.Task{Status: &v1alpha1.TaskStatus{State: state}}
		if got := taskPhase(task); got != want {
			t.Errorf("taskPhase(state %v) = %q, want %q", state, got, want)
		}
		task.Metadata = &v1alpha1.ObjectMeta{DeleteTime: timestamppb.Now()}
		if got := taskPhase(task); got != v1alpha1.ReasonTerminating {
			t.Errorf("taskPhase(state %v with delete_time) = %q, want %q", state, got, v1alpha1.ReasonTerminating)
		}
	}
	if got := taskPhase(nil); got != "" {
		t.Errorf("taskPhase(nil) = %q, want \"\"", got)
	}
}
