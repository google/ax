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
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/ax/internal/controller"
	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// slowControlServer is a Substrate stand-in that adds a fixed latency to every RPC
// on the reconcile path and records the peak number of concurrent calls per actor.
type slowControlServer struct {
	ateapipb.UnimplementedControlServer
	latency  time.Duration
	workerIP string

	mu         sync.Mutex
	inFlight   map[string]int
	maxPerTask int
	resumes    int
}

func (s *slowControlServer) enter(actor string) func() {
	s.mu.Lock()
	s.inFlight[actor]++
	s.maxPerTask = max(s.maxPerTask, s.inFlight[actor])
	s.mu.Unlock()
	time.Sleep(s.latency)
	return func() {
		s.mu.Lock()
		s.inFlight[actor]--
		s.mu.Unlock()
	}
}

func (s *slowControlServer) CreateAtespace(ctx context.Context, req *ateapipb.CreateAtespaceRequest) (*ateapipb.Atespace, error) {
	time.Sleep(s.latency)
	return &ateapipb.Atespace{}, nil
}

func (s *slowControlServer) GetActorTemplate(ctx context.Context, req *ateapipb.GetActorTemplateRequest) (*ateapipb.ActorTemplate, error) {
	time.Sleep(s.latency)
	return nil, status.Error(codes.NotFound, "not found")
}

func (s *slowControlServer) CreateActorTemplate(ctx context.Context, req *ateapipb.CreateActorTemplateRequest) (*ateapipb.ActorTemplate, error) {
	time.Sleep(s.latency)
	return req.GetActorTemplate(), nil
}

func (s *slowControlServer) CreateActor(ctx context.Context, req *ateapipb.CreateActorRequest) (*ateapipb.Actor, error) {
	defer s.enter(req.GetActor().GetMetadata().GetName())()
	return &ateapipb.Actor{
		Metadata: req.GetActor().GetMetadata(),
		Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED},
	}, nil
}

func (s *slowControlServer) CreateActorEgressPolicy(ctx context.Context, req *ateapipb.CreateActorEgressPolicyRequest) (*ateapipb.EgressPolicy, error) {
	defer s.enter(req.GetActor().GetName())()
	return &ateapipb.EgressPolicy{}, nil
}

func (s *slowControlServer) ResumeActor(ctx context.Context, req *ateapipb.ResumeActorRequest) (*ateapipb.ResumeActorResponse, error) {
	defer s.enter(req.GetActor().GetName())()
	s.mu.Lock()
	s.resumes++
	s.mu.Unlock()
	return &ateapipb.ResumeActorResponse{
		Actor: &ateapipb.Actor{
			Metadata: &ateapipb.ResourceMetadata{Name: req.GetActor().GetName()},
			Status: &ateapipb.ActorStatus{
				State:            ateapipb.ActorState_ACTOR_STATE_RUNNING,
				WorkerAssignment: &ateapipb.WorkerAssignment{WorkerPodIp: s.workerIP},
			},
		},
	}, nil
}

type workerHarness struct {
	store  *memory.MemoryStore
	server *slowControlServer
	worker *controller.Worker
}

// newWorkerHarness wires a worker to a slow Substrate and a runner /readyz that
// reports the workspace ready immediately (warm) or never (cold, so every
// reconcile polls until readyTimeout).
func newWorkerHarness(tb testing.TB, concurrency int, latency, readyTimeout time.Duration, warm bool) *workerHarness {
	tb.Helper()
	readyz := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !warm {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	tb.Cleanup(readyz.Close)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("failed to listen: %v", err)
	}
	srv := &slowControlServer{
		latency:  latency,
		workerIP: strings.TrimPrefix(readyz.URL, "http://"),
		inFlight: map[string]int{},
	}
	grpcServer := grpc.NewServer()
	ateapipb.RegisterControlServer(grpcServer, srv)
	go grpcServer.Serve(lis)
	tb.Cleanup(grpcServer.Stop)

	subClient, err := substrate.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		tb.Fatalf("failed to create substrate client: %v", err)
	}
	tb.Cleanup(func() { subClient.Close() })

	reconciler := controller.NewTaskReconciler(subClient, "default-template", "ax-system")
	reconciler.SecretResolver = noSecrets
	reconciler.WorkspaceReadyTimeout = readyTimeout

	memStore := memory.NewStore()
	worker := controller.NewWorker(memStore, reconciler, "bench", "bench-1")
	worker.Concurrency = concurrency
	return &workerHarness{store: memStore, server: srv, worker: worker}
}

// applyAndWait saves the named tasks, then waits until every one has been
// reconciled (its status reports a worker IP).
func (h *workerHarness) applyAndWait(tb testing.TB, ctx context.Context, names []string, timeout time.Duration) {
	tb.Helper()
	for _, name := range names {
		if err := h.store.SaveTask(ctx, &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: name, Atespace: "default"},
			Spec:     &v1alpha1.TaskSpec{Image: "ghcr.io/test/img"},
		}); err != nil {
			tb.Fatalf("SaveTask(%s): %v", name, err)
		}
	}
	deadline := time.Now().Add(timeout)
	for _, name := range names {
		for {
			task, err := h.store.GetTask(ctx, "default", name)
			if err == nil && task.GetStatus().GetWorkerIp() != "" {
				break
			}
			if time.Now().After(deadline) {
				tb.Fatalf("task %s not reconciled within %v", name, timeout)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// BenchmarkWorkerThroughput measures how many freshly applied tasks one controller
// process reconciles per second. Substrate RPCs take 10ms each; in the cold case
// the workspace never reports ready, so every reconcile also waits out a 200ms
// readiness poll (scaled down from the 15s default).
//
//	go test ./internal/controller -run '^$' -bench WorkerThroughput -benchtime 1x
func BenchmarkWorkerThroughput(b *testing.B) {
	const batch = 64
	for _, warm := range []bool{true, false} {
		for _, concurrency := range []int{1, 4, 16, 64} {
			workspace := "cold"
			if warm {
				workspace = "warm"
			}
			b.Run(fmt.Sprintf("workspace=%s/concurrency=%d", workspace, concurrency), func(b *testing.B) {
				h := newWorkerHarness(b, concurrency, 10*time.Millisecond, 200*time.Millisecond, warm)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				go func() { _ = h.worker.Run(ctx) }()

				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					names := make([]string, batch)
					for j := range names {
						names[j] = fmt.Sprintf("task-%d-%d", i, j)
					}
					h.applyAndWait(b, ctx, names, 5*time.Minute)
				}
				b.ReportMetric(float64(batch*b.N)/b.Elapsed().Seconds(), "tasks/s")
			})
		}
	}
}

// TestWorkerConcurrencyKeepsTasksSerial applies several revisions of each task at
// once and checks that no task ever has two reconciles in flight, while distinct
// tasks still make progress in parallel.
func TestWorkerConcurrencyKeepsTasksSerial(t *testing.T) {
	h := newWorkerHarness(t, 8, 20*time.Millisecond, 50*time.Millisecond, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.worker.Run(ctx) }()

	const tasks, revisions = 16, 3
	start := time.Now()
	// Queue every revision up front, back to back, so the same task sits in the
	// queue several times in a row.
	for i := 0; i < tasks; i++ {
		for rev := 0; rev < revisions; rev++ {
			if err := h.store.SaveTask(ctx, &v1alpha1.Task{
				Metadata: &v1alpha1.ObjectMeta{Name: fmt.Sprintf("serial-%d", i), Atespace: "default"},
				Spec:     &v1alpha1.TaskSpec{Image: fmt.Sprintf("ghcr.io/test/img:%d", rev)},
			}); err != nil {
				t.Fatalf("SaveTask: %v", err)
			}
		}
	}
	for {
		h.server.mu.Lock()
		resumes, peak := h.server.resumes, h.server.maxPerTask
		h.server.mu.Unlock()
		if resumes == tasks*revisions {
			if peak != 1 {
				t.Errorf("a task had %d concurrent Substrate calls, want 1", peak)
			}
			break
		}
		if time.Since(start) > 30*time.Second {
			t.Fatalf("only %d of %d reconciles finished", resumes, tasks*revisions)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Serially, 48 reconciles of 6 RPCs at 20ms each take about 5.8s.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("48 reconciles took %v with concurrency 8; tasks do not appear to run in parallel", elapsed)
	}
}
