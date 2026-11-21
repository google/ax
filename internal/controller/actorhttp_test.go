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

package controller_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/ax/internal/controller"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

// fakeRunner serves the runner's push and workspace check endpoints.
type fakeRunner struct {
	pushCode  int    // reply to the setup push; 0 means 202
	busyPolls int32  // how many workspace checks answer 503 first
	result    int    // workspace check reply after busyPolls; 0 means 200
	reason    string // body of a failed workspace check
	pushes    atomic.Int32
	polls     atomic.Int32
	body      atomic.Value
}

func (f *fakeRunner) start(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+v1alpha1.RunnerSetupPath, func(w http.ResponseWriter, r *http.Request) {
		f.pushes.Add(1)
		b, _ := io.ReadAll(r.Body)
		f.body.Store(string(b))
		if f.pushCode != 0 {
			http.Error(w, "busy with other work", f.pushCode)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("check") != "workspace" {
			t.Errorf("readyz polled without check=workspace")
		}
		if f.polls.Add(1) <= f.busyPolls {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if f.result != 0 && f.result != http.StatusOK {
			http.Error(w, f.reason, f.result)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func newTestSetupRunner() *controller.HTTPSetupRunner {
	r := controller.NewHTTPSetupRunner()
	r.AcceptTimeout = 2 * time.Second
	r.PollInterval = 10 * time.Millisecond
	return r
}

func setupWorkspace() *v1alpha1.Workspace {
	return &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Atespace: "default", Name: "repo", Uid: "ws-uid"},
		Spec:     &v1alpha1.WorkspaceSpec{},
	}
}

func TestHTTPSetupRunner_PushesAndWaits(t *testing.T) {
	t.Setenv("ATENET_ROUTER_ADDR", "")
	f := &fakeRunner{busyPolls: 2}
	addr := f.start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := newTestSetupRunner().RunSetup(ctx, "default", "setup-1", addr, setupWorkspace()); err != nil {
		t.Fatalf("RunSetup: %v", err)
	}
	if got := f.pushes.Load(); got != 1 {
		t.Errorf("pushes = %d, want 1", got)
	}
	if got := f.polls.Load(); got != 3 {
		t.Errorf("workspace checks = %d, want 3", got)
	}
	if body, _ := f.body.Load().(string); !strings.Contains(body, "name: repo") {
		t.Errorf("pushed body %q does not carry the Workspace", body)
	}
}

func TestHTTPSetupRunner_SetupFailed(t *testing.T) {
	t.Setenv("ATENET_ROUTER_ADDR", "")
	f := &fakeRunner{result: http.StatusInternalServerError, reason: "git clone failed"}
	addr := f.start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := newTestSetupRunner().RunSetup(ctx, "default", "setup-1", addr, setupWorkspace())
	if err == nil || !strings.Contains(err.Error(), "git clone failed") {
		t.Fatalf("RunSetup error = %v, want the runner's reason", err)
	}
}

func TestHTTPSetupRunner_RejectedPushIsNotRetried(t *testing.T) {
	t.Setenv("ATENET_ROUTER_ADDR", "")
	f := &fakeRunner{pushCode: http.StatusConflict}
	addr := f.start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := newTestSetupRunner().RunSetup(ctx, "default", "setup-1", addr, setupWorkspace())
	if err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("RunSetup error = %v, want the 409 rejection", err)
	}
	if got := f.pushes.Load(); got != 1 {
		t.Errorf("pushes = %d, want 1 (a 4xx is not retried)", got)
	}
	if got := f.polls.Load(); got != 0 {
		t.Errorf("workspace checks = %d, want 0 after a rejected push", got)
	}
}

func TestHTTPSetupRunner_NoAddress(t *testing.T) {
	t.Setenv("ATENET_ROUTER_ADDR", "")
	start := time.Now()
	err := newTestSetupRunner().RunSetup(context.Background(), "default", "setup-1", "", setupWorkspace())
	if err == nil || !strings.Contains(err.Error(), "no address") {
		t.Fatalf("RunSetup error = %v, want no address", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("RunSetup took %v; an actor without an address must fail at once", time.Since(start))
	}
}

func TestHTTPSetupRunner_FallsBackToRouter(t *testing.T) {
	var target atomic.Value
	f := &fakeRunner{}
	routerAddr := f.start(t)
	// Wrap the fake runner to record the routing header.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		target.Store(r.Header.Get("ate-target-actor"))
		proxy, _ := http.NewRequestWithContext(r.Context(), r.Method, "http://"+routerAddr+r.URL.RequestURI(), r.Body)
		resp, err := http.DefaultClient.Do(proxy)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
	router := httptest.NewServer(mux)
	defer router.Close()
	t.Setenv("ATENET_ROUTER_ADDR", router.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 127.0.0.1:1 refuses connections, so the request goes through the router.
	if err := newTestSetupRunner().RunSetup(ctx, "default", "setup-1", "127.0.0.1:1", setupWorkspace()); err != nil {
		t.Fatalf("RunSetup: %v", err)
	}
	if got, _ := target.Load().(string); got != "default/setup-1" {
		t.Errorf("ate-target-actor = %q, want default/setup-1", got)
	}
}
