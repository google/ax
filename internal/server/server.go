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
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/ax/internal/lock"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Reconciler coordinates sandbox/actor lifecycles on Agent Substrate directly.
type Reconciler interface {
	Reconcile(ctx context.Context, task *v1alpha1.Task, ws *v1alpha1.Workspace) (*v1alpha1.Task, error)
	ReconcileDelete(ctx context.Context, atespace, taskName string) error
}

// Observer fills in the parts of a task's status that are owned by its actor
// on Agent Substrate. The store never holds them, so every task the API returns
// passes through the Observer first.
type Observer interface {
	Observe(ctx context.Context, tasks []*v1alpha1.Task)
}

// Options configures the AX API Server.
type Options struct {
	Locker     lock.Locker
	Reconciler Reconciler
	Observer   Observer
}

// watchPollInterval is how often WatchTask re-reads a task's actor, so changes
// made on Substrate (for example a crash) reach watchers without an AX write.
var watchPollInterval = 2 * time.Second

// Server provides the gRPC API for AX.
type Server struct {
	v1alpha1.UnimplementedAXServer
	store      store.Store
	locker     lock.Locker
	reconciler Reconciler
	observer   Observer
	grpcServer *grpc.Server
}

// NewServer creates a new AX API server.
func NewServer(s store.Store, opts ...Options) *Server {
	var opt Options
	if len(opts) > 0 {
		opt = opts[0]
	}
	locker := opt.Locker
	if locker == nil {
		locker = lock.NewMemoryLocker()
	}

	srv := &Server{
		store:      s,
		locker:     locker,
		reconciler: opt.Reconciler,
		observer:   opt.Observer,
		grpcServer: grpc.NewServer(),
	}
	v1alpha1.RegisterAXServer(srv.grpcServer, srv)
	return srv
}

// GRPCServer returns the underlying gRPC server instance.
func (s *Server) GRPCServer() *grpc.Server {
	return s.grpcServer
}

// Handler returns the HTTP handler for the server, routing gRPC and HTTP health checks.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			s.grpcServer.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		http.NotFound(w, r)
	})
}

// --- gRPC AXServer implementation ---

// --- Tasks ---

func (s *Server) GetTask(ctx context.Context, req *v1alpha1.GetTaskRequest) (*v1alpha1.Task, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing request")
	}
	atespace := req.Atespace
	if atespace == "" {
		atespace = "default"
	}
	task, err := s.store.GetTask(ctx, atespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "task %q not found in atespace %q", req.Name, atespace)
		}
		return nil, status.Errorf(codes.Internal, "getting task: %v", err)
	}
	s.observe(ctx, task)
	return task, nil
}

func (s *Server) ListTasks(ctx context.Context, req *v1alpha1.ListTasksRequest) (*v1alpha1.ListTasksResponse, error) {
	atespace := ""
	limit := int64(50)
	offset := int64(0)
	if req != nil {
		atespace = req.Atespace
		if req.Limit > 0 {
			limit = req.Limit
		}
		if req.Offset >= 0 {
			offset = req.Offset
		}
	}
	tasks, err := s.store.ListTasks(ctx, atespace, limit, offset)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing tasks: %v", err)
	}
	s.observe(ctx, tasks...)
	return &v1alpha1.ListTasksResponse{Tasks: tasks}, nil
}

func (s *Server) CreateTask(ctx context.Context, req *v1alpha1.CreateTaskRequest) (*v1alpha1.Task, error) {
	if req == nil || req.Task == nil {
		return nil, status.Error(codes.InvalidArgument, "task required")
	}
	task := req.Task
	if err := v1alpha1.ValidateTask(task); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if task.Metadata == nil {
		task.Metadata = &v1alpha1.ObjectMeta{}
	}
	atespace := task.Metadata.Atespace
	if atespace == "" {
		atespace = "default"
		task.Metadata.Atespace = atespace
	}
	taskName := task.Metadata.GetName()

	// Acquire exclusive lock for this task
	unlock, err := s.locker.Lock(ctx, "task", atespace, taskName)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "locking task %s/%s: %v", atespace, taskName, err)
	}
	defer unlock()

	existing, err := s.store.GetTask(ctx, atespace, taskName)
	if err == nil {
		if existing.GetStatus().GetPhase() != v1alpha1.PhasePending || !proto.Equal(existing.GetSpec(), task.GetSpec()) {
			return nil, status.Errorf(codes.FailedPrecondition, "task %s/%s already exists and is immutable", atespace, taskName)
		}
		task = existing
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.Internal, "checking existing task: %v", err)
	}

	if task.Metadata.CreateTime == nil {
		task.Metadata.CreateTime = timestamppb.Now()
	}
	if task.Status == nil {
		task.Status = &v1alpha1.TaskStatus{}
	}
	// The task is Pending until its actor exists. New tasks start suspended.
	task.Status.Phase = v1alpha1.PhasePending
	if err := s.store.SaveTask(ctx, withStoredStatus(task)); err != nil {
		return nil, status.Errorf(codes.Internal, "saving task: %v", err)
	}
	task.Status.Phase = "Suspended"

	// Directly reconcile with Substrate
	if s.reconciler != nil {
		reconciled, err := s.reconciler.Reconcile(ctx, task, s.fetchWorkspace(ctx, atespace, task))
		if err != nil {
			slog.Error("direct reconcile error on create task", "task", taskName, "error", err)
			if reconciled != nil && reconciled.Status != nil {
				task.Status = reconciled.Status
			}
			if substrate.IsTransientError(err) {
				task.Status.Phase = v1alpha1.PhasePending
			} else if task.Status.Phase != v1alpha1.PhasePending {
				task.Status.Phase = v1alpha1.PhaseFailed
			}
			_ = s.store.UpdateTaskStatus(ctx, atespace, taskName, storedStatus(task.Status))
			if substrate.IsTransientError(err) {
				return nil, status.Errorf(codes.Unavailable, "provisioning task on substrate: %v", err)
			}
			return nil, status.Errorf(codes.Internal, "provisioning task on substrate: %v", err)
		}
		task.Status = reconciled.Status
	}
	if err := s.store.UpdateTaskStatus(ctx, atespace, taskName, storedStatus(task.Status)); err != nil {
		return nil, status.Errorf(codes.Internal, "updating task status: %v", err)
	}

	return task, nil
}

func (s *Server) DeleteTask(ctx context.Context, req *v1alpha1.DeleteTaskRequest) (*v1alpha1.DeleteTaskResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing request")
	}
	atespace := req.Atespace
	if atespace == "" {
		atespace = "default"
	}
	taskName := req.Name

	// Acquire exclusive lock for this task
	unlock, err := s.locker.Lock(ctx, "task", atespace, taskName)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "locking task %s/%s: %v", atespace, taskName, err)
	}
	defer unlock()

	task, err := s.store.GetTask(ctx, atespace, taskName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "task %q not found in atespace %q", taskName, atespace)
		}
		return nil, status.Errorf(codes.Internal, "getting task: %v", err)
	}

	// Mark the task as terminating while actor cleanup is in progress.
	if task.Status == nil {
		task.Status = &v1alpha1.TaskStatus{}
	}
	task.Status.Phase = v1alpha1.PhaseTerminating
	if err := s.store.UpdateTaskStatus(ctx, atespace, taskName, storedStatus(task.Status)); err != nil {
		return nil, status.Errorf(codes.Internal, "updating task status: %v", err)
	}

	// Directly clean up Substrate actor and templates
	if s.reconciler != nil {
		if err := s.reconciler.ReconcileDelete(ctx, atespace, taskName); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil, status.Error(codes.Canceled, err.Error())
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, status.Error(codes.DeadlineExceeded, err.Error())
			}
			if st, ok := status.FromError(err); ok && st.Code() != codes.Unknown {
				return nil, err
			}
			return nil, status.Errorf(codes.Internal, "cleaning up substrate resources for task %s/%s: %v", atespace, taskName, err)
		}
	}

	if err := s.store.DeleteTask(ctx, atespace, taskName); err != nil {
		return nil, status.Errorf(codes.Internal, "deleting task record: %v", err)
	}
	return &v1alpha1.DeleteTaskResponse{}, nil
}

func (s *Server) SuspendTask(ctx context.Context, req *v1alpha1.SuspendTaskRequest) (*v1alpha1.Task, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing request")
	}
	atespace := req.Atespace
	if atespace == "" {
		atespace = "default"
	}
	taskName := req.Name

	// Acquire exclusive lock for this task
	unlock, err := s.locker.Lock(ctx, "task", atespace, taskName)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "locking task %s/%s: %v", atespace, taskName, err)
	}
	defer unlock()

	task, err := s.store.GetTask(ctx, atespace, taskName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "task %q not found in atespace %q", taskName, atespace)
		}
		return nil, status.Errorf(codes.Internal, "getting task: %v", err)
	}
	if task.Status == nil {
		task.Status = &v1alpha1.TaskStatus{}
	}
	task.Status.Phase = "Suspended"

	if s.reconciler != nil {
		reconciled, err := s.reconciler.Reconcile(ctx, task, s.fetchWorkspace(ctx, atespace, task))
		if err != nil {
			if substrate.IsTransientError(err) {
				return nil, status.Errorf(codes.Unavailable, "suspending task on substrate: %v", err)
			}
			return nil, status.Errorf(codes.Internal, "suspending task on substrate: %v", err)
		}
		task.Status = reconciled.Status
		if err := s.store.UpdateTaskStatus(ctx, atespace, taskName, storedStatus(task.Status)); err != nil {
			return nil, status.Errorf(codes.Internal, "updating task status: %v", err)
		}
	} else {
		if err := s.store.UpdateTaskStatus(ctx, atespace, taskName, storedStatus(task.Status)); err != nil {
			return nil, status.Errorf(codes.Internal, "suspending task: %v", err)
		}
	}

	return task, nil
}

func (s *Server) ResumeTask(ctx context.Context, req *v1alpha1.ResumeTaskRequest) (*v1alpha1.Task, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing request")
	}
	atespace := req.Atespace
	if atespace == "" {
		atespace = "default"
	}
	taskName := req.Name

	// Acquire exclusive lock for this task
	unlock, err := s.locker.Lock(ctx, "task", atespace, taskName)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "locking task %s/%s: %v", atespace, taskName, err)
	}
	defer unlock()

	task, err := s.store.GetTask(ctx, atespace, taskName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "task %q not found in atespace %q", taskName, atespace)
		}
		return nil, status.Errorf(codes.Internal, "getting task: %v", err)
	}
	if task.Status == nil {
		task.Status = &v1alpha1.TaskStatus{}
	}
	task.Status.Phase = "Running"

	if s.reconciler != nil {
		reconciled, err := s.reconciler.Reconcile(ctx, task, s.fetchWorkspace(ctx, atespace, task))
		if err != nil {
			if reconciled != nil && reconciled.Status != nil {
				task.Status = reconciled.Status
			}
			if substrate.IsTransientError(err) {
				task.Status.Phase = v1alpha1.PhasePending
			} else if task.Status.Phase != v1alpha1.PhasePending {
				task.Status.Phase = v1alpha1.PhaseFailed
			}
			_ = s.store.UpdateTaskStatus(ctx, atespace, taskName, storedStatus(task.Status))
			if substrate.IsTransientError(err) {
				return nil, status.Errorf(codes.Unavailable, "resuming task on substrate: %v", err)
			}
			return nil, status.Errorf(codes.Internal, "resuming task on substrate: %v", err)
		}
		task.Status = reconciled.Status
		if err := s.store.UpdateTaskStatus(ctx, atespace, taskName, storedStatus(task.Status)); err != nil {
			return nil, status.Errorf(codes.Internal, "updating task status: %v", err)
		}
	} else {
		if err := s.store.UpdateTaskStatus(ctx, atespace, taskName, storedStatus(task.Status)); err != nil {
			return nil, status.Errorf(codes.Internal, "resuming task: %v", err)
		}
	}

	return task, nil
}

// observe fills in the actor-owned parts of the tasks' status, when the server
// is connected to Substrate.
func (s *Server) observe(ctx context.Context, tasks ...*v1alpha1.Task) {
	if s.observer != nil && len(tasks) > 0 {
		s.observer.Observe(ctx, tasks)
	}
}

// storedStatus returns the part of a task's status that AX owns and stores. The
// actor, its worker, and the phase and Ready condition that follow from the
// actor's state are dropped: they are read from Substrate instead, so the store
// can never hold a stale copy of them.
func storedStatus(st *v1alpha1.TaskStatus) *v1alpha1.TaskStatus {
	out := &v1alpha1.TaskStatus{}
	if st != nil {
		out = proto.Clone(st).(*v1alpha1.TaskStatus)
	}
	out.Actor = ""
	out.WorkerIp = ""
	if !v1alpha1.IsAXPhase(out.Phase) {
		out.Phase = ""
		conds := out.Conditions[:0]
		for _, c := range out.Conditions {
			if c.GetType() != "Ready" {
				conds = append(conds, c)
			}
		}
		out.Conditions = conds
	}
	return out
}

// withStoredStatus returns a copy of task whose status is storedStatus.
func withStoredStatus(task *v1alpha1.Task) *v1alpha1.Task {
	out := proto.Clone(task).(*v1alpha1.Task)
	out.Status = storedStatus(task.Status)
	return out
}

// fetchWorkspace loads the Workspace the task binds, or nil when the task
// binds none or the Workspace does not exist.
func (s *Server) fetchWorkspace(ctx context.Context, atespace string, task *v1alpha1.Task) *v1alpha1.Workspace {
	name := task.GetSpec().GetWorkspace().GetName()
	if name == "" {
		return nil
	}
	ws, err := s.store.GetWorkspace(ctx, atespace, name)
	if err != nil {
		return nil
	}
	return ws
}

func (s *Server) WatchTask(req *v1alpha1.WatchTaskRequest, stream grpc.ServerStreamingServer[v1alpha1.WatchTaskResponse]) error {
	if req == nil {
		return status.Error(codes.InvalidArgument, "missing request")
	}
	atespace := req.Atespace
	if atespace == "" {
		atespace = "default"
	}
	ctx := stream.Context()
	ch, closer, err := s.store.WatchTask(ctx, atespace, req.Name)
	if err != nil {
		return status.Errorf(codes.Internal, "watching task: %v", err)
	}
	defer closer.Close()

	var last *v1alpha1.Task
	send := func(task *v1alpha1.Task, action string) (done bool, err error) {
		s.observe(ctx, task)
		if last != nil && proto.Equal(last, task) {
			return false, nil
		}
		last = task
		if err := stream.Send(&v1alpha1.WatchTaskResponse{Task: task, Action: action}); err != nil {
			return true, err
		}
		phase := task.GetStatus().GetPhase()
		return phase == v1alpha1.PhaseFailed || phase == "Completed", nil
	}

	if initial, err := s.store.GetTask(ctx, atespace, req.Name); err == nil {
		if done, err := send(initial, "INITIAL"); done || err != nil {
			return err
		}
	}

	// Store events cover AX's own writes. Polling picks up changes that happen
	// on Substrate alone, such as an actor crashing or being suspended.
	ticker := time.NewTicker(watchPollInterval)
	defer ticker.Stop()
	for {
		var task *v1alpha1.Task
		select {
		case <-ctx.Done():
			return ctx.Err()
		case t, ok := <-ch:
			if !ok {
				return nil
			}
			task = t
		case <-ticker.C:
			t, err := s.store.GetTask(ctx, atespace, req.Name)
			if err != nil {
				continue
			}
			task = t
		}
		if done, err := send(task, "MODIFIED"); done || err != nil {
			return err
		}
	}
}

// --- Workspaces ---

func (s *Server) GetWorkspace(ctx context.Context, req *v1alpha1.GetWorkspaceRequest) (*v1alpha1.Workspace, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing request")
	}
	atespace := req.Atespace
	if atespace == "" {
		atespace = "default"
	}
	ws, err := s.store.GetWorkspace(ctx, atespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "workspace %q not found in atespace %q", req.Name, atespace)
		}
		return nil, status.Errorf(codes.Internal, "getting workspace: %v", err)
	}
	return ws, nil
}

func (s *Server) ListWorkspaces(ctx context.Context, req *v1alpha1.ListWorkspacesRequest) (*v1alpha1.ListWorkspacesResponse, error) {
	atespace := ""
	if req != nil {
		atespace = req.Atespace
	}
	workspaces, err := s.store.ListWorkspaces(ctx, atespace)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing workspaces: %v", err)
	}
	return &v1alpha1.ListWorkspacesResponse{Workspaces: workspaces}, nil
}

func (s *Server) UpdateWorkspace(ctx context.Context, req *v1alpha1.UpdateWorkspaceRequest) (*v1alpha1.Workspace, error) {
	if req == nil || req.Workspace == nil {
		return nil, status.Error(codes.InvalidArgument, "workspace required")
	}
	if err := v1alpha1.ValidateWorkspace(req.Workspace); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	req.Workspace.Metadata = defaultMetadata(req.Workspace.Metadata, func(atespace, name string) *v1alpha1.ObjectMeta {
		existing, err := s.store.GetWorkspace(ctx, atespace, name)
		if err != nil {
			return nil
		}
		return existing.GetMetadata()
	})

	atespace := req.Workspace.Metadata.Atespace
	wsName := req.Workspace.Metadata.Name

	// Acquire exclusive lock for this workspace
	unlock, err := s.locker.Lock(ctx, "workspace", atespace, wsName)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "locking workspace %s/%s: %v", atespace, wsName, err)
	}
	defer unlock()

	if err := s.store.SaveWorkspace(ctx, req.Workspace); err != nil {
		return nil, status.Errorf(codes.Internal, "saving workspace: %v", err)
	}
	return req.Workspace, nil
}

func (s *Server) DeleteWorkspace(ctx context.Context, req *v1alpha1.DeleteWorkspaceRequest) (*v1alpha1.DeleteWorkspaceResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing request")
	}
	atespace := req.Atespace
	if atespace == "" {
		atespace = "default"
	}
	wsName := req.Name

	// Acquire exclusive lock for this workspace
	unlock, err := s.locker.Lock(ctx, "workspace", atespace, wsName)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "locking workspace %s/%s: %v", atespace, wsName, err)
	}
	defer unlock()

	if err := s.store.DeleteWorkspace(ctx, atespace, wsName); err != nil {
		return nil, status.Errorf(codes.Internal, "deleting workspace: %v", err)
	}
	return &v1alpha1.DeleteWorkspaceResponse{}, nil
}

// --- Models ---

func (s *Server) GetModel(ctx context.Context, req *v1alpha1.GetModelRequest) (*v1alpha1.Model, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing request")
	}
	atespace := req.Atespace
	if atespace == "" {
		atespace = "default"
	}
	model, err := s.store.GetModel(ctx, atespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "model %q not found in atespace %q", req.Name, atespace)
		}
		return nil, status.Errorf(codes.Internal, "getting model: %v", err)
	}
	return model, nil
}

func (s *Server) ListModels(ctx context.Context, req *v1alpha1.ListModelsRequest) (*v1alpha1.ListModelsResponse, error) {
	atespace := ""
	if req != nil {
		atespace = req.Atespace
	}
	models, err := s.store.ListModels(ctx, atespace)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing models: %v", err)
	}
	return &v1alpha1.ListModelsResponse{Models: models}, nil
}

func (s *Server) UpdateModel(ctx context.Context, req *v1alpha1.UpdateModelRequest) (*v1alpha1.Model, error) {
	if req == nil || req.Model == nil {
		return nil, status.Error(codes.InvalidArgument, "model required")
	}
	if err := v1alpha1.ValidateModel(req.Model); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	req.Model.Metadata = defaultMetadata(req.Model.Metadata, func(atespace, name string) *v1alpha1.ObjectMeta {
		existing, err := s.store.GetModel(ctx, atespace, name)
		if err != nil {
			return nil
		}
		return existing.GetMetadata()
	})

	atespace := req.Model.Metadata.Atespace
	modelName := req.Model.Metadata.Name

	// Acquire exclusive lock for this model
	unlock, err := s.locker.Lock(ctx, "model", atespace, modelName)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "locking model %s/%s: %v", atespace, modelName, err)
	}
	defer unlock()

	if err := s.store.SaveModel(ctx, req.Model); err != nil {
		return nil, status.Errorf(codes.Internal, "saving model: %v", err)
	}
	return req.Model, nil
}

func (s *Server) DeleteModel(ctx context.Context, req *v1alpha1.DeleteModelRequest) (*v1alpha1.DeleteModelResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing request")
	}
	atespace := req.Atespace
	if atespace == "" {
		atespace = "default"
	}
	modelName := req.Name

	// Acquire exclusive lock for this model
	unlock, err := s.locker.Lock(ctx, "model", atespace, modelName)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "locking model %s/%s: %v", atespace, modelName, err)
	}
	defer unlock()

	if err := s.store.DeleteModel(ctx, atespace, modelName); err != nil {
		return nil, status.Errorf(codes.Internal, "deleting model: %v", err)
	}
	return &v1alpha1.DeleteModelResponse{}, nil
}

func defaultMetadata(meta *v1alpha1.ObjectMeta, existing func(atespace, name string) *v1alpha1.ObjectMeta) *v1alpha1.ObjectMeta {
	if meta == nil {
		meta = &v1alpha1.ObjectMeta{}
	}
	if meta.Atespace == "" {
		meta.Atespace = "default"
	}
	if meta.CreateTime == nil {
		var prev *v1alpha1.ObjectMeta
		if existing != nil {
			prev = existing(meta.Atespace, meta.Name)
		}
		if prev.GetCreateTime() != nil {
			meta.CreateTime = prev.GetCreateTime()
		} else {
			meta.CreateTime = timestamppb.Now()
		}
	}
	return meta
}
