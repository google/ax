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
	"net/http"
	"strings"

	"github.com/google/ax/internal/store"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Server provides the gRPC API for AX.
type Server struct {
	v1alpha1.UnimplementedAXServer
	store      store.Store
	grpcServer *grpc.Server
}

// NewServer creates a new AX API server.
func NewServer(s store.Store) *Server {
	srv := &Server{
		store:      s,
		grpcServer: grpc.NewServer(),
	}
	v1alpha1.RegisterAXServer(srv.grpcServer, srv)
	return srv
}

// GRPCServer returns the underlying gRPC server.
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
	if task.Metadata.Atespace == "" {
		task.Metadata.Atespace = "default"
	}
	if task.Metadata.CreationTimestamp == nil {
		task.Metadata.CreationTimestamp = timestamppb.Now()
	}
	if err := s.store.CreateTask(ctx, task); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return nil, status.Errorf(codes.AlreadyExists, "task %q already exists in atespace %q", task.Metadata.Name, task.Metadata.Atespace)
		}
		return nil, status.Errorf(codes.Internal, "creating task: %v", err)
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
	task, err := s.store.GetTask(ctx, atespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "task %q not found in atespace %q", req.Name, atespace)
		}
		return nil, status.Errorf(codes.Internal, "getting task: %v", err)
	}
	if task.Status == nil {
		task.Status = &v1alpha1.TaskStatus{}
	}
	task.Status.Phase = v1alpha1.PhaseTerminating
	if err := s.store.UpdateTaskStatus(ctx, atespace, req.Name, task.Status); err != nil {
		return nil, status.Errorf(codes.Internal, "updating task status: %v", err)
	}
	if err := s.store.PublishEvent(ctx, store.TaskEvent{
		Atespace: atespace,
		Name:     req.Name,
		Action:   "delete",
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "publishing delete event: %v", err)
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
	task, err := s.store.GetTask(ctx, atespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "task %q not found in atespace %q", req.Name, atespace)
		}
		return nil, status.Errorf(codes.Internal, "getting task: %v", err)
	}
	if task.Status == nil {
		task.Status = &v1alpha1.TaskStatus{}
	}
	task.Status.Phase = v1alpha1.PhaseSuspended
	if err := s.store.UpdateTaskStatus(ctx, atespace, req.Name, task.Status); err != nil {
		return nil, status.Errorf(codes.Internal, "updating task status: %v", err)
	}
	if err := s.store.PublishEvent(ctx, store.TaskEvent{
		Atespace: atespace,
		Name:     req.Name,
		Action:   "suspend",
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "publishing suspend event: %v", err)
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
	task, err := s.store.GetTask(ctx, atespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "task %q not found in atespace %q", req.Name, atespace)
		}
		return nil, status.Errorf(codes.Internal, "getting task: %v", err)
	}
	if task.Status == nil {
		task.Status = &v1alpha1.TaskStatus{}
	}
	task.Status.Phase = v1alpha1.PhasePending
	if err := s.store.UpdateTaskStatus(ctx, atespace, req.Name, task.Status); err != nil {
		return nil, status.Errorf(codes.Internal, "updating task status: %v", err)
	}
	if err := s.store.PublishEvent(ctx, store.TaskEvent{
		Atespace: atespace,
		Name:     req.Name,
		Action:   "resume",
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "publishing resume event: %v", err)
	}
	return task, nil
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

	if initial, err := s.store.GetTask(ctx, atespace, req.Name); err == nil {
		if err := stream.Send(&v1alpha1.WatchTaskResponse{Task: initial, Action: "INITIAL"}); err != nil {
			return err
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case task, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(&v1alpha1.WatchTaskResponse{Task: task, Action: "MODIFIED"}); err != nil {
				return err
			}
			if task.Status != nil && (task.Status.Phase == "Running" || task.Status.Phase == "Failed" || task.Status.Phase == "Completed") {
				return nil
			}
		}
	}
}

// --- Gateways ---

func (s *Server) GetGateway(ctx context.Context, req *v1alpha1.GetGatewayRequest) (*v1alpha1.Gateway, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing request")
	}
	atespace := req.Atespace
	if atespace == "" {
		atespace = "default"
	}
	gw, err := s.store.GetGateway(ctx, atespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "gateway %q not found in atespace %q", req.Name, atespace)
		}
		return nil, status.Errorf(codes.Internal, "getting gateway: %v", err)
	}
	return gw, nil
}

func (s *Server) ListGateways(ctx context.Context, req *v1alpha1.ListGatewaysRequest) (*v1alpha1.ListGatewaysResponse, error) {
	atespace := ""
	if req != nil {
		atespace = req.Atespace
	}
	gateways, err := s.store.ListGateways(ctx, atespace)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing gateways: %v", err)
	}
	return &v1alpha1.ListGatewaysResponse{Gateways: gateways}, nil
}

func (s *Server) UpdateGateway(ctx context.Context, req *v1alpha1.UpdateGatewayRequest) (*v1alpha1.Gateway, error) {
	if req == nil || req.Gateway == nil {
		return nil, status.Error(codes.InvalidArgument, "gateway required")
	}
	req.Gateway.Metadata = defaultMetadata(req.Gateway.Metadata, func(atespace, name string) *v1alpha1.ObjectMeta {
		existing, err := s.store.GetGateway(ctx, atespace, name)
		if err != nil {
			return nil
		}
		return existing.GetMetadata()
	})
	if err := s.store.SaveGateway(ctx, req.Gateway); err != nil {
		return nil, status.Errorf(codes.Internal, "saving gateway: %v", err)
	}
	return req.Gateway, nil
}

func (s *Server) DeleteGateway(ctx context.Context, req *v1alpha1.DeleteGatewayRequest) (*v1alpha1.DeleteGatewayResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing request")
	}
	atespace := req.Atespace
	if atespace == "" {
		atespace = "default"
	}
	if err := s.store.DeleteGateway(ctx, atespace, req.Name); err != nil {
		return nil, status.Errorf(codes.Internal, "deleting gateway: %v", err)
	}
	return &v1alpha1.DeleteGatewayResponse{}, nil
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
	req.Workspace.Metadata = defaultMetadata(req.Workspace.Metadata, func(atespace, name string) *v1alpha1.ObjectMeta {
		existing, err := s.store.GetWorkspace(ctx, atespace, name)
		if err != nil {
			return nil
		}
		return existing.GetMetadata()
	})
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
	if err := s.store.DeleteWorkspace(ctx, atespace, req.Name); err != nil {
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
	req.Model.Metadata = defaultMetadata(req.Model.Metadata, func(atespace, name string) *v1alpha1.ObjectMeta {
		existing, err := s.store.GetModel(ctx, atespace, name)
		if err != nil {
			return nil
		}
		return existing.GetMetadata()
	})
	if err := s.store.SaveModel(ctx, req.Model); err != nil {
		return nil, status.Errorf(codes.Internal, "saving model: %v", err)
	}
	return req.Model, nil
}

// defaultMetadata normalizes resource metadata before a save: a missing atespace
// becomes "default", and the creation timestamp is carried over from the existing
// resource (looked up via existing) or set to now for a new one.
func defaultMetadata(meta *v1alpha1.ObjectMeta, existing func(atespace, name string) *v1alpha1.ObjectMeta) *v1alpha1.ObjectMeta {
	if meta == nil {
		meta = &v1alpha1.ObjectMeta{}
	}
	if meta.Atespace == "" {
		meta.Atespace = "default"
	}
	if meta.CreationTimestamp == nil {
		if prev := existing(meta.Atespace, meta.Name); prev.GetCreationTimestamp() != nil {
			meta.CreationTimestamp = prev.GetCreationTimestamp()
		} else {
			meta.CreationTimestamp = timestamppb.Now()
		}
	}
	return meta
}

func (s *Server) DeleteModel(ctx context.Context, req *v1alpha1.DeleteModelRequest) (*v1alpha1.DeleteModelResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing request")
	}
	atespace := req.Atespace
	if atespace == "" {
		atespace = "default"
	}
	if err := s.store.DeleteModel(ctx, atespace, req.Name); err != nil {
		return nil, status.Errorf(codes.Internal, "deleting model: %v", err)
	}
	return &v1alpha1.DeleteModelResponse{}, nil
}
