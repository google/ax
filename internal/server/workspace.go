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

	"github.com/google/ax/internal/apivalidation"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

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
	if err := apivalidation.ValidateWorkspace(req.Workspace); err != nil {
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
