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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/ax/internal/controller"
	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/internal/substrate/substratetest"
	"github.com/google/ax/internal/tasklogs"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type logSourceFunc func(context.Context, string, string, time.Time) (io.ReadCloser, error)

func (f logSourceFunc) Open(ctx context.Context, ns, pod string, since time.Time) (io.ReadCloser, error) {
	return f(ctx, ns, pod, since)
}

func logClient(t *testing.T, source tasklogs.Source, actor *ateapipb.Actor) (v1alpha1.AXClient, *substratetest.Server) {
	t.Helper()
	st := memory.NewStore()
	if err := st.SaveTask(t.Context(), &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Name: "demo", Atespace: "default"}}); err != nil {
		t.Fatal(err)
	}
	sub := substratetest.NewServer(t)
	if actor != nil {
		sub.SetActor(actor)
	}
	srv := NewServer(st, controller.NewTaskReconciler(sub.Client(), "test", "default", ""), Options{TaskLogs: source})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.GRPCServer().Serve(listener) }()
	t.Cleanup(srv.GRPCServer().Stop)
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return v1alpha1.NewAXClient(conn), sub
}

func logActor(state ateapipb.ActorState, pod string) *ateapipb.Actor {
	return &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Name: "demo", Atespace: "default", Uid: "uid"}, Status: &ateapipb.ActorStatus{State: state, WorkerAssignment: &ateapipb.WorkerAssignment{WorkerNamespace: "workers", WorkerPod: pod}}}
}

func TestTaskLogsAdmission(t *testing.T) {
	running := ateapipb.ActorState_ACTOR_STATE_RUNNING
	noUID := logActor(running, "a")
	noUID.Metadata.Uid = ""
	for _, tt := range []struct {
		name    string
		enabled bool
		actor   *ateapipb.Actor
		task    string
		want    codes.Code
	}{
		{"disabled", false, logActor(running, "a"), "demo", codes.Unimplemented},
		{"invalid name", true, logActor(running, "a"), "../bad", codes.InvalidArgument},
		{"missing task", true, logActor(running, "a"), "missing", codes.NotFound},
		{"missing actor", true, nil, "demo", codes.NotFound},
		{"suspended", true, logActor(ateapipb.ActorState_ACTOR_STATE_SUSPENDED, "a"), "demo", codes.FailedPrecondition},
		{"crashed", true, logActor(ateapipb.ActorState_ACTOR_STATE_CRASHED, "a"), "demo", codes.FailedPrecondition},
		{"missing assignment", true, logActor(running, ""), "demo", codes.FailedPrecondition},
		{"missing UID", true, noUID, "demo", codes.FailedPrecondition},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var opened atomic.Int32
			var source tasklogs.Source
			if tt.enabled {
				source = logSourceFunc(func(context.Context, string, string, time.Time) (io.ReadCloser, error) {
					opened.Add(1)
					return nil, status.Error(codes.Internal, "unexpected open")
				})
			}
			client, sub := logClient(t, source, tt.actor)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			stream, err := client.StreamTaskLogs(ctx, &v1alpha1.StreamTaskLogsRequest{Name: tt.task})
			if err == nil {
				_, err = stream.Recv()
			}
			if status.Code(err) != tt.want {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
			if opened.Load() != 0 {
				t.Fatal("opened logs before admission")
			}
			if tt.actor != nil {
				actor, _ := sub.Actor("default", "demo")
				if actor.GetStatus().GetState() != tt.actor.GetStatus().GetState() {
					t.Fatal("log request changed actor state")
				}
			}
		})
	}
}

func TestTaskLogsFollowMovesAndCancel(t *testing.T) {
	closed := make(chan string, 4)
	var opens atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing service account token")
		}
		opens.Add(1)
		q := r.URL.Query()
		since, err := time.Parse(time.RFC3339Nano, q.Get("sinceTime"))
		if err != nil || time.Since(since) < 0 || time.Since(since) > 5*time.Second || q.Get("follow") != "true" || q.Get("container") != "worker" || q.Has("sinceSeconds") || q.Has("tailLines") {
			t.Errorf("unexpected log options: %v", q)
		}
		fmt.Fprintln(w, `{"labels":{"ate.actor.uid":"another-actor"},"message":"must not leak"}`)
		fmt.Fprintf(w, `{"labels":{"ate.actor.uid":"uid"},"message":%q}`+"\n", r.URL.Path)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		closed <- r.URL.Path
	}))
	t.Cleanup(api.Close)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	client, sub := logClient(t, &tasklogs.Client{URL: api.URL, HTTP: api.Client(), TokenFile: token}, logActor(ateapipb.ActorState_ACTOR_STATE_RUNNING, "a"))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	stream, err := client.StreamTaskLogs(ctx, &v1alpha1.StreamTaskLogsRequest{Name: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	recv := func(pod string) {
		t.Helper()
		msg, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(msg.Line, "/pods/"+pod+"/log") {
			t.Fatalf("unexpected record: %s", msg.Line)
		}
	}
	waitClosed := func(pod string) {
		t.Helper()
		select {
		case path := <-closed:
			if !strings.Contains(path, "/pods/"+pod+"/log") {
				t.Fatalf("closed %s, want %s", path, pod)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("log stream was not closed")
		}
	}
	recv("a")
	time.Sleep(1200 * time.Millisecond)
	if opens.Load() != 1 {
		t.Fatalf("opened %d streams for one unchanged worker", opens.Load())
	}
	sub.SetActor(logActor(ateapipb.ActorState_ACTOR_STATE_RUNNING, "b"))
	recv("b")
	waitClosed("a")
	sub.SetActor(logActor(ateapipb.ActorState_ACTOR_STATE_SUSPENDED, "b"))
	waitClosed("b")
	actor, _ := sub.Actor("default", "demo")
	if actor.Status.State != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatal("woke suspended actor")
	}
	sub.SetActor(logActor(ateapipb.ActorState_ACTOR_STATE_RUNNING, "c"))
	recv("c")
	cancel()
	waitClosed("c")
}

func TestTaskLogsErrorsAndRecreation(t *testing.T) {
	for _, tt := range []struct {
		name     string
		openErr  error
		text     string
		recreate bool
		want     codes.Code
	}{
		{name: "permission denied", openErr: status.Error(codes.PermissionDenied, "denied"), want: codes.PermissionDenied},
		{name: "upstream unavailable", openErr: status.Error(codes.Unavailable, "offline"), want: codes.Unavailable},
		{name: "record too large", text: strings.Repeat("x", 1024*1024+1), want: codes.Unavailable},
		{name: "recreated actor", recreate: true, want: codes.Aborted},
	} {
		t.Run(tt.name, func(t *testing.T) {
			closed := make(chan struct{})
			opened := make(chan struct{})
			source := logSourceFunc(func(ctx context.Context, _ string, _ string, _ time.Time) (io.ReadCloser, error) {
				if tt.openErr != nil {
					return nil, tt.openErr
				}
				close(opened)
				pr, pw := io.Pipe()
				go func() {
					if tt.text != "" {
						_, _ = io.WriteString(pw, tt.text+"\n")
					}
					<-ctx.Done()
					pw.Close()
					close(closed)
				}()
				return pr, nil
			})
			client, sub := logClient(t, source, logActor(ateapipb.ActorState_ACTOR_STATE_RUNNING, "a"))
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			stream, err := client.StreamTaskLogs(ctx, &v1alpha1.StreamTaskLogsRequest{Name: "demo"})
			if tt.recreate {
				select {
				case <-opened:
				case <-ctx.Done():
					t.Fatal("stream did not open")
				}
				actor := logActor(ateapipb.ActorState_ACTOR_STATE_RUNNING, "a")
				actor.Metadata.Uid = "replacement"
				sub.SetActor(actor)
			}
			if err == nil {
				_, err = stream.Recv()
			}
			if status.Code(err) != tt.want {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
			if tt.openErr == nil {
				select {
				case <-closed:
				case <-ctx.Done():
					t.Fatal("leaked reader")
				}
			}
		})
	}
}
