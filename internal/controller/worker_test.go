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
	"net"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/ax/internal/controller"
	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestWorkerReconciliation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Start in-process mock Substrate server
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	mockSrv := &mockControlServer{}
	grpcServer := grpc.NewServer()
	ateapipb.RegisterControlServer(grpcServer, mockSrv)
	go grpcServer.Serve(lis)
	defer grpcServer.Stop()

	// 2. Substrate client
	subClient, err := substrate.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer subClient.Close()

	reconciler := controller.NewTaskReconciler(subClient, "default-template", "ax-system")
	reconciler.SecretResolver = noSecrets
	reconciler.WorkspaceReadyTimeout = 200 * time.Millisecond

	// 3. In-memory store
	memStore := memory.NewStore()

	// Save task
	task := &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "worker-task", Atespace: "default"},
		Spec: &v1alpha1.TaskSpec{
			Image: "ghrc.io/test/img",
		},
	}
	if err := memStore.SaveTask(ctx, task); err != nil {
		t.Fatalf("failed to save task: %v", err)
	}

	// 4. Start the worker in the background
	worker := controller.NewWorker(memStore, reconciler, "test-group", "worker-1")
	go func() {
		_ = worker.Run(ctx)
	}()

	// 5. Poll store until task reaches "Running" phase
	deadline := time.Now().Add(3 * time.Second)
	var finalTask *v1alpha1.Task
	for time.Now().Before(deadline) {
		tItem, err := memStore.GetTask(ctx, "default", "worker-task")
		if err == nil && tItem.Status.Phase == "Running" {
			finalTask = tItem
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if finalTask == nil {
		t.Fatalf("task did not transition to Running phase in time")
	}

	if finalTask.Status.Actor != "worker-task" {
		t.Errorf("expected actor 'worker-task', got %q", finalTask.Status.Actor)
	}
	if finalTask.Status.WorkerIp != "10.244.1.42" {
		t.Errorf("expected worker IP '10.244.1.42', got %q", finalTask.Status.WorkerIp)
	}
}

func TestWorkerDeletion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	mockSrv := &mockControlServer{actorTemplates: map[string]bool{"doomed-tmpl-0a1b2c3d": true}}
	grpcServer := grpc.NewServer()
	ateapipb.RegisterControlServer(grpcServer, mockSrv)
	go grpcServer.Serve(lis)
	defer grpcServer.Stop()

	subClient, err := substrate.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer subClient.Close()

	reconciler := controller.NewTaskReconciler(subClient, "default-template", "ax-system")
	reconciler.SecretResolver = noSecrets
	reconciler.WorkspaceReadyTimeout = 200 * time.Millisecond

	memStore := memory.NewStore()
	task := &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "doomed", Atespace: "default"},
		Spec:     &v1alpha1.TaskSpec{Image: "ghcr.io/test/img"},
		Status:   &v1alpha1.TaskStatus{Phase: "Running", Actor: "doomed"},
	}
	if err := memStore.SaveTask(ctx, task); err != nil {
		t.Fatalf("failed to save task: %v", err)
	}
	// Drain the reconcile event SaveTask published so only the delete is processed.
	drain, _ := memStore.Subscribe(ctx, "drain", "drain")
	drainCtx, drainCancel := context.WithTimeout(ctx, time.Second)
	_, _ = drain.Next(drainCtx)
	drainCancel()

	if err := memStore.MarkTaskDeleting(ctx, "default", "doomed"); err != nil {
		t.Fatalf("MarkTaskDeleting failed: %v", err)
	}
	marked, err := memStore.GetTask(ctx, "default", "doomed")
	if err != nil {
		t.Fatalf("GetTask after mark failed: %v", err)
	}
	if marked.Status.Phase != v1alpha1.PhaseTerminating {
		t.Fatalf("expected phase Terminating, got %q", marked.Status.Phase)
	}

	worker := controller.NewWorker(memStore, reconciler, "test-group", "worker-1")
	go func() { _ = worker.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := memStore.GetTask(ctx, "default", "doomed"); err != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := memStore.GetTask(ctx, "default", "doomed"); err == nil {
		t.Fatalf("expected task record to be removed after cleanup")
	}
	if len(mockSrv.deletedActors) != 1 || mockSrv.deletedActors[0] != "doomed" {
		t.Errorf("expected actor 'doomed' deleted, got %v", mockSrv.deletedActors)
	}
	if len(mockSrv.deletedTemplates) != 1 || mockSrv.deletedTemplates[0] != "doomed-tmpl-0a1b2c3d" {
		t.Errorf("expected template deleted, got %v", mockSrv.deletedTemplates)
	}
}

func TestWorkerSkipsReconcileOfTerminatingTask(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	mockSrv := &mockControlServer{}
	grpcServer := grpc.NewServer()
	ateapipb.RegisterControlServer(grpcServer, mockSrv)
	go grpcServer.Serve(lis)
	defer grpcServer.Stop()

	subClient, err := substrate.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to create substrate client: %v", err)
	}
	defer subClient.Close()

	reconciler := controller.NewTaskReconciler(subClient, "default-template", "ax-system")
	reconciler.SecretResolver = noSecrets
	reconciler.WorkspaceReadyTimeout = 200 * time.Millisecond

	// Queue a reconcile and then a delete before the worker starts, as when a
	// task is deleted while the controller is still busy with other events.
	memStore := memory.NewStore()
	task := &v1alpha1.Task{
		Metadata: &v1alpha1.ObjectMeta{Name: "doomed", Atespace: "default"},
		Spec:     &v1alpha1.TaskSpec{Image: "ghcr.io/test/img"},
	}
	if err := memStore.SaveTask(ctx, task); err != nil {
		t.Fatalf("failed to save task: %v", err)
	}
	if err := memStore.MarkTaskDeleting(ctx, "default", "doomed"); err != nil {
		t.Fatalf("MarkTaskDeleting failed: %v", err)
	}

	worker := controller.NewWorker(memStore, reconciler, "test-group", "worker-1")
	go func() { _ = worker.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := memStore.GetTask(ctx, "default", "doomed"); err != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := memStore.GetTask(ctx, "default", "doomed"); err == nil {
		t.Fatalf("expected task record to be removed after cleanup")
	}
	if len(mockSrv.createdActors) != 0 || len(mockSrv.resumedActors) != 0 {
		t.Errorf("terminating task was reconciled: created %v, resumed %v", mockSrv.createdActors, mockSrv.resumedActors)
	}
}
