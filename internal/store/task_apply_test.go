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

package store_test

import (
	"testing"
	"time"

	"github.com/google/ax/internal/lifecycle"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
)

func TestTaskRowLifecycleRoundTripAndScrubbing(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

	if got := (store.TaskKey{Name: "t1"}).Normalized(); got.Atespace != v1alpha1.DefaultAtespace || got.Name != "t1" {
		t.Fatalf("TaskKey.Normalized() = %+v", got)
	}

	var row store.TaskRow
	state := lifecycle.State{
		Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Atespace: "payments", Name: "fix-bug"},
			Spec:     &v1alpha1.TaskSpec{SessionId: "sess-1"},
			Status: &v1alpha1.TaskStatus{
				State:    v1alpha1.TaskState_TASK_STATE_QUEUED,
				Actor:    "RUNNING",
				ActorUid: "live-uid-must-not-persist",
			},
		},
		Actions: []lifecycle.Action{{
			ID:   "a-1",
			Kind: lifecycle.ActionEnsureTemplate,
		}},
		ActorUID:           "actor-uid-1",
		ActiveRunID:        "r-1",
		RunSeq:             1,
		ActionSeq:          1,
		RunnerTokenHash:    []byte{1, 2, 3},
		LastAckedRunnerSeq: 4,
	}

	row.SetLifecycleState(state, now)
	if row.Task.GetStatus().GetActor() != "" || row.Task.GetStatus().GetActorUid() != "" {
		t.Fatalf("expected live status.actor and status.actor_uid scrubbed, got %+v", row.Task.GetStatus())
	}
	if row.Task.GetSpec().GetSessionId() != "sess-1" || row.NextReconcileAt == nil || !row.NextReconcileAt.Equal(now) {
		t.Fatalf("SessionId=%q NextReconcileAt=%v, want sess-1 and %v", row.Task.GetSpec().GetSessionId(), row.NextReconcileAt, now)
	}

	row.EgressRules = []*ateapipb.EgressRule{{
		Https: &ateapipb.HttpsRule{Hostnames: []string{"example.com"}},
	}}
	cp := row.Clone()
	cp.EgressRules[0].Https.Hostnames[0] = "mutated.com"
	cp.RunnerTokenHash[0] = 99
	cp.Actions[0].Tag = "mutated-tag"
	if row.EgressRules[0].GetHttps().GetHostnames()[0] != "example.com" ||
		row.RunnerTokenHash[0] != 1 || row.Actions[0].Tag != "" {
		t.Fatal("TaskRow.Clone shared mutable slices with original")
	}

	extracted := row.LifecycleState()
	if extracted.ActorUID != "actor-uid-1" || extracted.ActiveRunID != "r-1" ||
		extracted.LastAckedRunnerSeq != 4 || len(extracted.Actions) != 1 {
		t.Fatalf("LifecycleState() = %+v", extracted)
	}

	// DeleteTaskRow is matched strictly by pointer identity.
	if store.DeleteTaskRow == nil || store.DeleteTaskRow.Clone() == store.DeleteTaskRow {
		t.Fatal("DeleteTaskRow clone must not match sentinel pointer identity")
	}
}
