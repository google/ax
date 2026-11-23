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

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/ax/internal/server"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/internal/substrate/substratetest"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// captureEvents sends slog.Default, where telemetry.Emit writes events while
// OTLP logs are off, to a buffer.
func captureEvents(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// rawStateEvents returns the ax.task.state_changed records in buf, and resets
// buf.
func rawStateEvents(t *testing.T, buf *bytes.Buffer) []map[string]any {
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
		if m["event"] == server.TaskStateChanged.Name {
			out = append(out, m)
		}
	}
	buf.Reset()
	return out
}

// stateEvents is rawStateEvents with only the attributes of each record.
func stateEvents(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	out := rawStateEvents(t, buf)
	for _, m := range out {
		for _, k := range []string{"time", "level", "msg", "event"} {
			delete(m, k)
		}
	}
	return out
}

// transitions returns the (previous phase, phase, actor UID) of each
// ax.task.state_changed record in buf, and resets buf. A missing attribute
// is "".
func transitions(t *testing.T, buf *bytes.Buffer) [][3]string {
	t.Helper()
	var out [][3]string
	for _, ev := range stateEvents(t, buf) {
		get := func(k string) string { s, _ := ev[k].(string); return s }
		out = append(out, [3]string{get("ax.task.previous_phase"), get("ax.task.phase"), get("ate.actor.uid")})
	}
	return out
}

func checkTransitions(t *testing.T, buf *bytes.Buffer, step string, want ...[3]string) {
	t.Helper()
	if got := transitions(t, buf); !reflect.DeepEqual(got, want) {
		t.Errorf("%s: (previous phase, phase, actor UID) of events:\ngot  %q\nwant %q", step, got, want)
	}
}

// failTaskWriteStore fails the task writes whose flag is set.
type failTaskWriteStore struct {
	store.Store
	failSave, failUpdate, failDelete bool
}

var errStoreWrite = errors.New("simulated store failure")

func (f *failTaskWriteStore) SaveTask(ctx context.Context, task *v1alpha1.Task) error {
	if f.failSave {
		return errStoreWrite
	}
	return f.Store.SaveTask(ctx, task)
}

func (f *failTaskWriteStore) UpdateTaskStatus(ctx context.Context, atespace, name string, st *v1alpha1.TaskStatus) error {
	if f.failUpdate {
		return errStoreWrite
	}
	return f.Store.UpdateTaskStatus(ctx, atespace, name, st)
}

func (f *failTaskWriteStore) DeleteTask(ctx context.Context, atespace, name string) error {
	if f.failDelete {
		return errStoreWrite
	}
	return f.Store.DeleteTask(ctx, atespace, name)
}

func newTaskRequest(name string) *v1alpha1.CreateTaskRequest {
	return &v1alpha1.CreateTaskRequest{Task: &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: name},
		Spec:     &v1alpha1.TaskSpec{Profile: &v1alpha1.Profile{Image: "alpine"}},
	}}
}

func actorUID(t *testing.T, sub *substratetest.Server, name string) string {
	t.Helper()
	actor, ok := sub.Actor("default", name)
	if !ok || actor.GetMetadata().GetUid() == "" {
		t.Fatalf("task %s has no actor with a UID", name)
	}
	return actor.GetMetadata().GetUid()
}

// TestTaskStateChanged_Attributes checks every attribute of the records of
// one task, from creation to deletion.
func TestTaskStateChanged_Attributes(t *testing.T) {
	buf := captureEvents(t)
	sub := substratetest.NewServer(t)
	srv := server.NewServer(memory.NewStore(), newTestTaskManager(sub))
	ctx := context.Background()

	task, err := srv.CreateTask(ctx, newTaskRequest("t1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Name: "t1"}); err != nil {
		t.Fatal(err)
	}
	ev := func(previous, phase string, kv ...string) map[string]any {
		m := map[string]any{"ax.task.name": "t1", "ate.atespace": "default", "ax.task.id": task.GetMetadata().GetUid(), "ax.task.phase": phase}
		if previous != "" {
			m["ax.task.previous_phase"] = previous
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	want := []map[string]any{
		ev("", server.PhaseProvisioned, "ate.actor.uid", task.GetStatus().GetActorUid()),
		ev(server.PhaseProvisioned, v1alpha1.ReasonTerminating),
		ev(v1alpha1.ReasonTerminating, server.PhaseDeleted),
	}
	if task.GetStatus().GetActorUid() == "" {
		t.Fatal("CreateTask returned no actor UID")
	}
	if got := stateEvents(t, buf); !reflect.DeepEqual(got, want) {
		t.Errorf("events:\ngot  %v\nwant %v", got, want)
	}
}

// TestTaskStateChanged_Transitions follows one task through each phase
// change. A write that keeps the phase emits nothing.
func TestTaskStateChanged_Transitions(t *testing.T) {
	buf := captureEvents(t)
	sub := substratetest.NewServer(t)
	srv := server.NewServer(memory.NewStore(), newTestTaskManager(sub))
	ctx := context.Background()
	const (
		queued      = "Queued"
		provisioned = server.PhaseProvisioned
		terminating = v1alpha1.ReasonTerminating
	)
	resume := func() error {
		_, err := srv.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Name: "t1"})
		return err
	}
	deleteTask := func() error {
		_, err := srv.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Name: "t1"})
		return err
	}

	if _, err := srv.CreateTask(ctx, newTaskRequest("t1")); err != nil {
		t.Fatal(err)
	}
	uid := actorUID(t, sub, "t1")
	checkTransitions(t, buf, "create", [3]string{"", provisioned, uid})

	if err := resume(); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.SuspendTask(ctx, &v1alpha1.SuspendTaskRequest{Name: "t1"}); err != nil {
		t.Fatal(err)
	}
	checkTransitions(t, buf, "resume and suspend")

	// A transient resume failure stores Queued, though the actor still exists.
	sub.SetErrors(substratetest.Errors{ResumeActor: status.Error(codes.DeadlineExceeded, "substrate timed out")})
	if err := resume(); status.Code(err) != codes.Unavailable {
		t.Fatalf("ResumeTask: got %v, want Unavailable", err)
	}
	checkTransitions(t, buf, "transient resume failure", [3]string{provisioned, queued, ""})
	sub.SetErrors(substratetest.Errors{})
	if err := resume(); err != nil {
		t.Fatal(err)
	}
	checkTransitions(t, buf, "resume after a transient failure", [3]string{queued, provisioned, uid})

	// A failed actor deletion leaves the task Terminating, and a resume
	// cancels the deletion.
	sub.SetErrors(substratetest.Errors{DeleteActor: status.Error(codes.Internal, "substrate delete failure")})
	if err := deleteTask(); err == nil {
		t.Fatal("DeleteTask succeeded, want the Substrate error")
	}
	checkTransitions(t, buf, "failed delete", [3]string{provisioned, terminating, ""})
	sub.SetErrors(substratetest.Errors{})
	if err := resume(); err != nil {
		t.Fatal(err)
	}
	checkTransitions(t, buf, "resume after a failed delete", [3]string{terminating, provisioned, uid})

	// The retry of a failed delete keeps the phase Terminating, then deletes
	// the record.
	sub.SetErrors(substratetest.Errors{DeleteActor: status.Error(codes.Internal, "substrate delete failure")})
	if err := deleteTask(); err == nil {
		t.Fatal("DeleteTask succeeded, want the Substrate error")
	}
	checkTransitions(t, buf, "failed delete", [3]string{provisioned, terminating, ""})
	sub.SetErrors(substratetest.Errors{})
	if err := deleteTask(); err != nil {
		t.Fatal(err)
	}
	checkTransitions(t, buf, "delete", [3]string{terminating, server.PhaseDeleted, ""})
}

// TestTaskStateChanged_Failures checks that a failed create and a failed
// store write emit nothing.
func TestTaskStateChanged_Failures(t *testing.T) {
	buf := captureEvents(t)
	sub := substratetest.NewServer(t)
	st := &failTaskWriteStore{Store: memory.NewStore()}
	srv := server.NewServer(st, newTestTaskManager(sub))
	ctx := context.Background()
	const provisioned = server.PhaseProvisioned

	// A create that fails on Substrate, or at the store, stores no task.
	sub.SetErrors(substratetest.Errors{CreateActor: status.Error(codes.Internal, "substrate create failure")})
	if _, err := srv.CreateTask(ctx, newTaskRequest("t1")); status.Code(err) != codes.Internal {
		t.Fatalf("CreateTask: got %v, want Internal", err)
	}
	sub.SetErrors(substratetest.Errors{})
	st.failSave = true
	if _, err := srv.CreateTask(ctx, newTaskRequest("t1")); status.Code(err) != codes.Internal {
		t.Fatalf("CreateTask: got %v, want Internal", err)
	}
	checkTransitions(t, buf, "failed creates")
	st.failSave = false

	if _, err := srv.CreateTask(ctx, newTaskRequest("t1")); err != nil {
		t.Fatal(err)
	}
	checkTransitions(t, buf, "create", [3]string{"", provisioned, actorUID(t, sub, "t1")})

	// A failed UpdateTaskStatus emits nothing: without it, this transient
	// resume failure would emit Provisioned -> Queued. The client retries
	// Unavailable with backoff, so use DeadlineExceeded.
	sub.SetErrors(substratetest.Errors{ResumeActor: status.Error(codes.DeadlineExceeded, "substrate timed out")})
	st.failUpdate = true
	if _, err := srv.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Name: "t1"}); status.Code(err) != codes.Unavailable {
		t.Fatalf("ResumeTask: got %v, want Unavailable", err)
	}
	checkTransitions(t, buf, "failed UpdateTaskStatus")
	st.failUpdate = false

	// A permanent resume failure stores no state, so the phase stays
	// Provisioned.
	sub.SetErrors(substratetest.Errors{ResumeActor: status.Error(codes.Internal, "substrate resume failure")})
	if _, err := srv.ResumeTask(ctx, &v1alpha1.ResumeTaskRequest{Name: "t1"}); status.Code(err) != codes.Internal {
		t.Fatalf("ResumeTask: got %v, want Internal", err)
	}
	checkTransitions(t, buf, "permanent resume failure")
	sub.SetErrors(substratetest.Errors{})

	// A failed SaveTask emits nothing.
	st.failSave = true
	if _, err := srv.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Name: "t1"}); err == nil {
		t.Fatal("DeleteTask succeeded, want the store error")
	}
	checkTransitions(t, buf, "failed SaveTask")
	st.failSave = false

	// A failed store DeleteTask emits Terminating but not Deleted.
	st.failDelete = true
	if _, err := srv.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Name: "t1"}); err == nil {
		t.Fatal("DeleteTask succeeded, want the store error")
	}
	checkTransitions(t, buf, "failed store DeleteTask", [3]string{provisioned, v1alpha1.ReasonTerminating, ""})
}

// TestTaskStateChanged_ActorUnreadable checks that a task that is created
// while Substrate cannot return its actor has a record without ate.actor.uid.
func TestTaskStateChanged_ActorUnreadable(t *testing.T) {
	buf := captureEvents(t)
	sub := substratetest.NewServer(t)
	srv := server.NewServer(memory.NewStore(), newTestTaskManager(sub))

	// Only the response and the event read a new task's actor. Internal is
	// not retried.
	sub.SetErrors(substratetest.Errors{GetActor: status.Error(codes.Internal, "substrate get actor failure")})
	if _, err := srv.CreateTask(context.Background(), newTaskRequest("t1")); err != nil {
		t.Fatal(err)
	}
	checkTransitions(t, buf, "create", [3]string{"", server.PhaseProvisioned, ""})
}

// TestTaskStateChanged_Timestamp checks that a record is stamped with the time
// of its write, not with the time at which ax-server emits it after it reads
// the actor.
func TestTaskStateChanged_Timestamp(t *testing.T) {
	buf := captureEvents(t)
	sub := substratetest.NewServer(t)
	srv := server.NewServer(memory.NewStore(), newTestTaskManager(sub))

	var (
		mu         sync.Mutex
		lastGetAt  time.Time
		getActorCt int
	)
	sub.SetOnGetActor(func() {
		mu.Lock()
		lastGetAt = time.Now()
		getActorCt++
		mu.Unlock()
		// Put a gap between the write and the emit, larger than the
		// millisecond precision of slog's time.
		time.Sleep(20 * time.Millisecond)
	})
	if _, err := srv.CreateTask(context.Background(), newTaskRequest("t1")); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if getActorCt == 0 {
		t.Fatal("CreateTask did not read the actor")
	}
	evs := rawStateEvents(t, buf)
	if len(evs) != 1 || evs[0]["ax.task.phase"] != server.PhaseProvisioned {
		t.Fatalf("events = %v, want one Provisioned record", evs)
	}
	at, err := time.Parse(time.RFC3339Nano, evs[0]["time"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if at.After(lastGetAt) {
		t.Errorf("Provisioned record time %v is after the actor read at %v, want the earlier write time", at, lastGetAt)
	}
}
