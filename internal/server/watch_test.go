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
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/ax/internal/server"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type watchStore struct {
	store.Store
	get   func(context.Context, string, string) (*v1alpha1.Task, error)
	watch func(context.Context, string, string) (<-chan *v1alpha1.Task, io.Closer, error)
}

func (s *watchStore) GetTask(ctx context.Context, atespace, name string) (*v1alpha1.Task, error) {
	return s.get(ctx, atespace, name)
}
func (s *watchStore) WatchTask(ctx context.Context, atespace, name string) (<-chan *v1alpha1.Task, io.Closer, error) {
	return s.watch(ctx, atespace, name)
}

type watchCloser struct {
	io.Closer
	calls  atomic.Int32
	closed chan struct{}
}

func (c *watchCloser) Close() error {
	calls := c.calls.Add(1)
	var err error
	if c.Closer != nil {
		err = c.Closer.Close()
	}
	if calls == 1 && c.closed != nil {
		close(c.closed)
	}
	return err
}

type watchStream struct {
	grpc.ServerStream
	ctx  context.Context
	send func(*v1alpha1.WatchTaskResponse) error
}

func (s *watchStream) Context() context.Context                 { return s.ctx }
func (s *watchStream) Send(r *v1alpha1.WatchTaskResponse) error { return s.send(r) }

func watchTask(phase string) *v1alpha1.Task {
	return &v1alpha1.Task{Metadata: &v1alpha1.ObjectMeta{Atespace: "default", Name: "watched"}, Status: &v1alpha1.TaskStatus{Phase: phase}}
}

func TestWatchTaskLifecycle(t *testing.T) {
	backendErr := errors.New("backend unavailable")
	sendErr := errors.New("send failed")
	for _, tc := range []struct {
		name             string
		getErr, watchErr error
		sendFail         string
		cancel           bool
		phase            string
		code             codes.Code
		wantErr          error
		wantActions      string
	}{
		{name: "missing", getErr: store.ErrNotFound, code: codes.NotFound},
		{name: "wrapped missing", getErr: fmt.Errorf("lookup: %w", store.ErrNotFound), code: codes.NotFound},
		{name: "lookup error", getErr: backendErr, code: codes.Internal},
		{name: "registration error", watchErr: backendErr, code: codes.Internal},
		{name: "initial send error", sendFail: "INITIAL", wantErr: sendErr, wantActions: "INITIAL"},
		{name: "modified send error", sendFail: "MODIFIED", wantErr: sendErr, wantActions: "INITIAL,MODIFIED"},
		{name: "cancellation", cancel: true, wantErr: context.Canceled, wantActions: "INITIAL"},
		{name: "channel closed", wantActions: "INITIAL,MODIFIED"},
		{name: "completed", phase: "Completed", wantActions: "INITIAL,MODIFIED"},
		{name: "failed", phase: "Failed", wantActions: "INITIAL,MODIFIED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			closer := &watchCloser{}
			ch := make(chan *v1alpha1.Task, 1)
			if !tc.cancel {
				ch <- watchTask(tc.phase)
			}
			if tc.phase == "" && !tc.cancel {
				close(ch)
			}
			registered, reads := false, 0
			s := &watchStore{
				watch: func(_ context.Context, atespace, name string) (<-chan *v1alpha1.Task, io.Closer, error) {
					if atespace != "default" || name != "watched" {
						t.Fatalf("watch scope = %s/%s", atespace, name)
					}
					registered = true
					return ch, closer, tc.watchErr
				},
				get: func(_ context.Context, atespace, name string) (*v1alpha1.Task, error) {
					reads++
					if !registered {
						t.Fatal("initial read before watch registration can lose updates")
					}
					if atespace != "default" || name != "watched" {
						t.Fatalf("lookup scope = %s/%s", atespace, name)
					}
					return watchTask("Running"), tc.getErr
				},
			}
			var actions []string
			stream := &watchStream{ctx: ctx, send: func(r *v1alpha1.WatchTaskResponse) error {
				actions = append(actions, r.Action)
				if r.Task == nil || r.Task.Metadata.Name != "watched" {
					t.Fatalf("invalid task frame: %v", r)
				}
				if r.Action == tc.sendFail {
					return sendErr
				}
				if tc.cancel {
					cancel()
				}
				return nil
			}}
			err := server.NewServer(s).WatchTask(&v1alpha1.WatchTaskRequest{Name: "watched"}, stream)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
			} else if status.Code(err) != tc.code {
				t.Fatalf("error = %v, want %v", err, tc.code)
			}
			wantReads, wantCloses := 1, int32(1)
			if tc.watchErr != nil {
				wantReads, wantCloses = 0, 0
			}
			if reads != wantReads || closer.calls.Load() != wantCloses {
				t.Fatalf("reads=%d closes=%d, want %d/%d", reads, closer.calls.Load(), wantReads, wantCloses)
			}
			if got := strings.Join(actions, ","); got != tc.wantActions {
				t.Fatalf("actions=%q, want %q", got, tc.wantActions)
			}
			if tc.code == codes.NotFound && status.Convert(err).Message() != `task "watched" not found in atespace "default"` {
				t.Fatalf("unexpected missing-task error: %v", err)
			}
		})
	}
	t.Run("nil request", func(t *testing.T) {
		err := server.NewServer(&watchStore{}).WatchTask(nil, nil)
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("error = %v", err)
		}
	})
}

func watchGRPCClient(t *testing.T, s store.Store) v1alpha1.AXClient {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := server.NewServer(s).GRPCServer()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		srv.Stop()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return v1alpha1.NewAXClient(conn)
}

func TestWatchTaskGRPCMissing(t *testing.T) {
	for _, atespace := range []string{"", "other"} {
		t.Run("atespace="+atespace, func(t *testing.T) {
			mem := memory.NewStore()
			t.Cleanup(func() { mem.Close() })
			if err := mem.SaveTask(context.Background(), watchTask("Running")); err != nil {
				t.Fatal(err)
			}
			client := watchGRPCClient(t, mem)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			t.Cleanup(cancel)
			name := "missing"
			if atespace == "other" {
				name = "watched"
			}
			stream, err := client.WatchTask(ctx, &v1alpha1.WatchTaskRequest{Atespace: atespace, Name: name})
			if err == nil {
				_, err = stream.Recv()
			}
			if status.Code(err) != codes.NotFound {
				t.Fatalf("missing watch: %v, want NotFound", err)
			}
		})
	}
}

func TestWatchTaskGRPCRegistrationAndCleanup(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminal=%t", terminal), func(t *testing.T) {
			mem := memory.NewStore()
			t.Cleanup(func() { mem.Close() })
			if err := mem.SaveTask(context.Background(), watchTask("Pending")); err != nil {
				t.Fatal(err)
			}
			closer := &watchCloser{closed: make(chan struct{})}
			s := &watchStore{Store: mem}
			s.watch = func(ctx context.Context, atespace, name string) (<-chan *v1alpha1.Task, io.Closer, error) {
				ch, c, err := mem.WatchTask(ctx, atespace, name)
				closer.Closer = c
				return ch, closer, err
			}
			s.get = func(ctx context.Context, atespace, name string) (*v1alpha1.Task, error) {
				initial, err := mem.GetTask(ctx, atespace, name)
				if err != nil {
					return nil, err
				}
				// Publish after taking the initial snapshot, before returning it.
				// Registration must precede this read to deliver the update.
				if err := mem.UpdateTaskStatus(ctx, atespace, name, &v1alpha1.TaskStatus{Phase: "Running"}); err != nil {
					return nil, err
				}
				return initial, nil
			}
			client := watchGRPCClient(t, s)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			t.Cleanup(cancel)
			stream, err := client.WatchTask(ctx, &v1alpha1.WatchTaskRequest{Name: "watched"})
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []struct{ action, phase string }{{"INITIAL", "Pending"}, {"MODIFIED", "Running"}} {
				r, err := stream.Recv()
				if err != nil {
					t.Fatal(err)
				}
				if r.Action != want.action || r.Task.Status.Phase != want.phase {
					t.Fatalf("frame = %v, want %v", r, want)
				}
			}
			if terminal {
				if err := mem.UpdateTaskStatus(ctx, "default", "watched", &v1alpha1.TaskStatus{Phase: "Completed"}); err != nil {
					t.Fatal(err)
				}
				r, err := stream.Recv()
				if err != nil || r.Action != "MODIFIED" || r.Task.Status.Phase != "Completed" {
					t.Fatalf("terminal frame = %v, %v", r, err)
				}
				if _, err := stream.Recv(); err != io.EOF {
					t.Fatalf("terminal Recv = %v, want EOF", err)
				}
			} else {
				cancel()
				if _, err := stream.Recv(); status.Code(err) != codes.Canceled {
					t.Fatalf("canceled Recv = %v", err)
				}
			}
			select {
			case <-closer.closed:
			case <-time.After(5 * time.Second):
				t.Fatal("subscription not closed")
			}
			if closer.calls.Load() != 1 {
				t.Fatalf("close calls = %d", closer.calls.Load())
			}
		})
	}
}

func TestWatchTaskGRPCErrors(t *testing.T) {
	for _, tc := range []struct {
		name             string
		getErr, watchErr error
		code             codes.Code
	}{
		{name: "missing", getErr: store.ErrNotFound, code: codes.NotFound},
		{name: "lookup error", getErr: errors.New("lookup failed"), code: codes.Internal},
		{name: "registration error", watchErr: errors.New("registration failed"), code: codes.Internal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closer := &watchCloser{closed: make(chan struct{})}
			s := &watchStore{
				get: func(context.Context, string, string) (*v1alpha1.Task, error) { return nil, tc.getErr },
				watch: func(context.Context, string, string) (<-chan *v1alpha1.Task, io.Closer, error) {
					return make(chan *v1alpha1.Task), closer, tc.watchErr
				},
			}
			client := watchGRPCClient(t, s)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			t.Cleanup(cancel)
			stream, err := client.WatchTask(ctx, &v1alpha1.WatchTaskRequest{Name: "watched"})
			if err == nil {
				_, err = stream.Recv()
			}
			if status.Code(err) != tc.code {
				t.Fatalf("error = %v, want %v", err, tc.code)
			}
			wantCloses := int32(1)
			if tc.watchErr != nil {
				wantCloses = 0
			}
			if closer.calls.Load() != wantCloses {
				t.Fatalf("close calls = %d, want %d", closer.calls.Load(), wantCloses)
			}
		})
	}
}
