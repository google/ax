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

package taskevents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/ax/internal/server"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var key = store.TaskKey{Name: "t1"}

// captureEvents routes slog.Default, where Emit writes events while OTLP logs
// are off, into a buffer.
func captureEvents(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// rawEvents returns the ax.task.state_changed records in buf and resets it.
func rawEvents(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("decoding %q: %v", line, err)
		}
		if m["event"] == StateChanged.Name {
			out = append(out, m)
		}
	}
	buf.Reset()
	return out
}

// events is rawEvents with only the event's attributes kept.
func events(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	out := rawEvents(t, buf)
	for _, m := range out {
		for _, k := range []string{"time", "level", "msg", "event"} {
			delete(m, k)
		}
	}
	return out
}

// storeOnly hides MemoryStore's TaskApplier so TaskApplierForStore uses its
// store.Store adapter.
type storeOnly struct{ store.Store }

func backends() map[string]func() store.TaskApplier {
	return map[string]func() store.TaskApplier{
		"applier": func() store.TaskApplier { return memory.NewStore() },
		"adapter": func() store.TaskApplier { return server.TaskApplierForStore(storeOnly{memory.NewStore()}) },
	}
}

func create(state v1alpha1.TaskState) store.ApplyFunc {
	return func(current *store.TaskRow, _ time.Time) (*store.TaskRow, error) {
		return &store.TaskRow{Task: &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Uid: "uid-1"},
			Status:   &v1alpha1.TaskStatus{State: state},
		}}, nil
	}
}

func update(edit func(*store.TaskRow)) store.ApplyFunc {
	return func(current *store.TaskRow, _ time.Time) (*store.TaskRow, error) {
		next := current.Clone()
		edit(next)
		return next, nil
	}
}

func toProvisioned(r *store.TaskRow) {
	r.Task.Status.State = v1alpha1.TaskState_TASK_STATE_UNSPECIFIED
}

func withActor(uid string) func(*store.TaskRow) {
	return func(r *store.TaskRow) { r.ActorUID = uid }
}

func toActive(r *store.TaskRow) {
	toProvisioned(r)
	withActor("actor-1")(r)
}

func toFailed(r *store.TaskRow) {
	r.Task.Status.State = v1alpha1.TaskState_TASK_STATE_FAILED
	r.Task.Status.Outcome = &v1alpha1.TaskOutcome{Result: v1alpha1.TaskResult_TASK_RESULT_FAILED, ExitCode: 3}
}

func deleteRow(*store.TaskRow, time.Time) (*store.TaskRow, error) { return store.DeleteTaskRow, nil }

func TestApplyEmitsOnPhaseOutcomeOrActorChange(t *testing.T) {
	queued := v1alpha1.TaskState_TASK_STATE_QUEUED
	tests := []struct {
		name    string
		setup   []store.ApplyFunc
		apply   store.ApplyFunc
		wantErr bool
		want    []map[string]any
	}{{
		name:  "create",
		apply: create(queued),
		want: []map[string]any{{
			"ax.task.name": "t1", "ate.atespace": "default", "ax.task.id": "uid-1", "ax.task.generation": float64(1),
			"ax.task.phase": "Queued",
		}},
	}, {
		name:  "queued to provisioned",
		setup: []store.ApplyFunc{create(queued)},
		apply: update(toActive),
		want: []map[string]any{{
			"ax.task.name": "t1", "ate.atespace": "default", "ax.task.id": "uid-1", "ax.task.generation": float64(2),
			"ate.actor.uid": "actor-1", "ax.task.phase": "Provisioned", "ax.task.previous_phase": "Queued",
		}},
	}, {
		name:  "actor bound without phase change",
		setup: []store.ApplyFunc{create(queued), update(toProvisioned)},
		apply: update(withActor("actor-1")),
		want: []map[string]any{{
			"ax.task.name": "t1", "ate.atespace": "default", "ax.task.id": "uid-1", "ax.task.generation": float64(3),
			"ate.actor.uid": "actor-1", "ax.task.phase": "Provisioned", "ax.task.previous_phase": "Provisioned",
		}},
	}, {
		name:  "actor rotated without phase change",
		setup: []store.ApplyFunc{create(queued), update(toActive)},
		apply: update(withActor("actor-2")),
		want: []map[string]any{{
			"ax.task.name": "t1", "ate.atespace": "default", "ax.task.id": "uid-1", "ax.task.generation": float64(3),
			"ate.actor.uid": "actor-2", "ax.task.phase": "Provisioned", "ax.task.previous_phase": "Provisioned",
		}},
	}, {
		name:  "actor cleared",
		setup: []store.ApplyFunc{create(queued), update(toActive)},
		apply: update(withActor("")),
	}, {
		name:  "provisioned to failed with outcome",
		setup: []store.ApplyFunc{create(queued), update(toActive)},
		apply: update(toFailed),
		want: []map[string]any{{
			"ax.task.name": "t1", "ate.atespace": "default", "ax.task.id": "uid-1", "ax.task.generation": float64(3),
			"ate.actor.uid": "actor-1", "ax.task.phase": "Failed", "ax.task.previous_phase": "Provisioned",
			"ax.task.outcome": "failed", "ax.task.exit_code": float64(3),
		}},
	}, {
		name:  "outcome change only",
		setup: []store.ApplyFunc{create(v1alpha1.TaskState_TASK_STATE_COMPLETED)},
		apply: update(func(r *store.TaskRow) {
			r.Task.Status.Outcome = &v1alpha1.TaskOutcome{Result: v1alpha1.TaskResult_TASK_RESULT_SUCCEEDED}
		}),
		want: []map[string]any{{
			"ax.task.name": "t1", "ate.atespace": "default", "ax.task.id": "uid-1", "ax.task.generation": float64(2),
			"ax.task.phase": "Completed", "ax.task.previous_phase": "Completed",
			"ax.task.outcome": "succeeded", "ax.task.exit_code": float64(0),
		}},
	}, {
		name:  "cancelled outcome has no exit code",
		setup: []store.ApplyFunc{create(queued)},
		apply: update(func(r *store.TaskRow) {
			r.Task.Status.State = v1alpha1.TaskState_TASK_STATE_CANCELLED
			r.Task.Status.Outcome = &v1alpha1.TaskOutcome{Result: v1alpha1.TaskResult_TASK_RESULT_CANCELLED}
		}),
		want: []map[string]any{{
			"ax.task.name": "t1", "ate.atespace": "default", "ax.task.id": "uid-1", "ax.task.generation": float64(2),
			"ax.task.phase": "Cancelled", "ax.task.previous_phase": "Queued", "ax.task.outcome": "cancelled",
		}},
	}, {
		name:  "terminating",
		setup: []store.ApplyFunc{create(queued), update(toActive)},
		apply: update(func(r *store.TaskRow) { r.Task.Metadata.DeleteTime = timestamppb.Now() }),
		want: []map[string]any{{
			"ax.task.name": "t1", "ate.atespace": "default", "ax.task.id": "uid-1", "ax.task.generation": float64(3),
			"ate.actor.uid": "actor-1", "ax.task.phase": "Terminating", "ax.task.previous_phase": "Provisioned",
		}},
	}, {
		name:  "delete keeps identity and previous outcome",
		setup: []store.ApplyFunc{create(queued), update(toActive), update(toFailed)},
		apply: deleteRow,
		want: []map[string]any{{
			"ax.task.name": "t1", "ate.atespace": "default", "ax.task.id": "uid-1", "ax.task.generation": float64(4),
			"ate.actor.uid": "actor-1", "ax.task.phase": "Deleted", "ax.task.previous_phase": "Failed",
			"ax.task.outcome": "failed", "ax.task.previous_outcome": "failed", "ax.task.exit_code": float64(3),
		}},
	}, {
		name:  "write without phase, outcome, or actor change",
		setup: []store.ApplyFunc{create(queued)},
		apply: update(func(r *store.TaskRow) { r.Runnable = true; r.Task.Status.Attempt = 2 }),
	}, {
		name:  "no-op returns current",
		setup: []store.ApplyFunc{create(queued)},
		apply: func(current *store.TaskRow, _ time.Time) (*store.TaskRow, error) { return current, nil },
	}, {
		name:  "no-op returns nil on existing row",
		setup: []store.ApplyFunc{create(queued)},
		apply: func(*store.TaskRow, time.Time) (*store.TaskRow, error) { return nil, nil },
	}, {
		name:  "no-op on missing row",
		apply: func(*store.TaskRow, time.Time) (*store.TaskRow, error) { return nil, nil },
	}, {
		name:  "delete missing row",
		apply: deleteRow,
	}, {
		name:  "fn error aborts the write",
		setup: []store.ApplyFunc{create(queued)},
		apply: func(current *store.TaskRow, now time.Time) (*store.TaskRow, error) {
			if _, err := update(toFailed)(current, now); err != nil {
				return nil, err
			}
			return nil, errors.New("abort")
		},
		wantErr: true,
	}, {
		name:  "fn error with delete aborts the write",
		setup: []store.ApplyFunc{create(queued)},
		apply: func(*store.TaskRow, time.Time) (*store.TaskRow, error) {
			return store.DeleteTaskRow, errors.New("abort")
		},
		wantErr: true,
	}}
	for bname, newBackend := range backends() {
		for _, tc := range tests {
			t.Run(bname+"/"+tc.name, func(t *testing.T) {
				buf := captureEvents(t)
				a := Wrap(newBackend())
				ctx := context.Background()
				for _, fn := range tc.setup {
					if _, err := a.Apply(ctx, key, fn); err != nil {
						t.Fatalf("setup Apply: %v", err)
					}
				}
				events(t, buf)

				if _, err := a.Apply(ctx, key, tc.apply); (err != nil) != tc.wantErr {
					t.Fatalf("Apply error = %v, wantErr %v", err, tc.wantErr)
				}
				got := events(t, buf)
				if len(got) != len(tc.want) {
					t.Fatalf("got %d events %v, want %d %v", len(got), got, len(tc.want), tc.want)
				}
				for i := range got {
					if !maps.Equal(got[i], tc.want[i]) {
						t.Errorf("event %d:\n got  %v\n want %v", i, got[i], tc.want[i])
					}
				}
			})
		}
	}
}

// fakeApplier calls fn once per row in currents, as a store that retries
// does, at commit time now, and commits the last result with err.
type fakeApplier struct {
	store.TaskApplier
	currents []*store.TaskRow
	now      time.Time
	err      error
}

func (f fakeApplier) Apply(ctx context.Context, key store.TaskKey, fn store.ApplyFunc) (*store.TaskRow, error) {
	var next *store.TaskRow
	for _, cur := range f.currents {
		var err error
		if next, err = fn(cur, f.now); err != nil {
			return nil, err
		}
	}
	if next != nil && next != store.DeleteTaskRow {
		next = next.Clone()
		next.Generation++
	}
	return next, f.err
}

func queuedRow() *store.TaskRow {
	return &store.TaskRow{Generation: 1, Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Uid: "uid-1"},
		Status:   &v1alpha1.TaskStatus{State: v1alpha1.TaskState_TASK_STATE_QUEUED},
	}}
}

func TestApplyDoesNotEmitOnFailedCommit(t *testing.T) {
	for name, fn := range map[string]store.ApplyFunc{"update": update(toFailed), "delete": deleteRow} {
		t.Run(name, func(t *testing.T) {
			buf := captureEvents(t)
			a := Wrap(fakeApplier{currents: []*store.TaskRow{queuedRow()}, err: errors.New("commit failed")})
			if _, err := a.Apply(context.Background(), key, fn); err == nil {
				t.Fatal("Apply succeeded, want commit error")
			}
			if got := events(t, buf); len(got) != 0 {
				t.Errorf("got events %v after a failed commit, want none", got)
			}
		})
	}
}

func TestApplyReportsRowReplacedByRetry(t *testing.T) {
	buf := captureEvents(t)
	// The first call loses an insert race; the retry sees the winner's row.
	a := Wrap(fakeApplier{currents: []*store.TaskRow{nil, queuedRow()}})
	if _, err := a.Apply(context.Background(), key, func(current *store.TaskRow, now time.Time) (*store.TaskRow, error) {
		if current == nil {
			return create(v1alpha1.TaskState_TASK_STATE_QUEUED)(nil, now)
		}
		return update(toFailed)(current, now)
	}); err != nil {
		t.Fatal(err)
	}
	got := events(t, buf)
	if len(got) != 1 || got[0]["ax.task.previous_phase"] != "Queued" || got[0]["ax.task.phase"] != "Failed" {
		t.Errorf("events = %v, want one Queued to Failed", got)
	}
}

func TestApplyStampsCommitTime(t *testing.T) {
	buf := captureEvents(t)
	commit := time.Date(2001, 2, 3, 4, 5, 6, 7, time.UTC)
	a := Wrap(fakeApplier{currents: []*store.TaskRow{queuedRow()}, now: commit})
	if _, err := a.Apply(context.Background(), key, update(toFailed)); err != nil {
		t.Fatal(err)
	}
	got := rawEvents(t, buf)
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if ts, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(got[0]["time"])); !ts.Equal(commit) {
		t.Errorf("event time = %v, want commit time %v", got[0]["time"], commit)
	}
}

func TestApplyConcurrent(t *testing.T) {
	const n = 20
	buf := captureEvents(t)
	a := Wrap(memory.NewStore())
	ctx := context.Background()
	if _, err := a.Apply(ctx, key, create(v1alpha1.TaskState_TASK_STATE_QUEUED)); err != nil {
		t.Fatal(err)
	}
	events(t, buf)

	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			k := store.TaskKey{Name: fmt.Sprintf("c%d", i)}
			if _, err := a.Apply(ctx, k, create(v1alpha1.TaskState_TASK_STATE_QUEUED)); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() {
			if _, err := a.Apply(ctx, key, update(func(r *store.TaskRow) { r.Task.Status.Attempt++ })); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := events(t, buf); len(got) != n {
		t.Errorf("got %d events, want %d (one per create)", len(got), n)
	}
}

func TestWrapNil(t *testing.T) {
	if Wrap(nil) != nil {
		t.Error("Wrap(nil) != nil")
	}
}

func TestGetTaskRowDelegates(t *testing.T) {
	a := Wrap(memory.NewStore())
	ctx := context.Background()
	if _, err := a.Apply(ctx, key, create(v1alpha1.TaskState_TASK_STATE_QUEUED)); err != nil {
		t.Fatal(err)
	}
	row, err := a.GetTaskRow(ctx, key)
	if err != nil || row.Task.GetMetadata().GetUid() != "uid-1" {
		t.Errorf("GetTaskRow = %v, %v; want the created row", row, err)
	}
}
