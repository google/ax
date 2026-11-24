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
	"time"

	"github.com/google/ax/internal/apivalidation"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

const workspaceStatusUpdateTimeout = 10 * time.Second

func (s *Server) GetWorkspace(ctx context.Context, req *v1alpha1.GetWorkspaceRequest) (*v1alpha1.Workspace, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing request")
	}
	atespace := atespaceOrDefault(req.Atespace)
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
	atespace := req.GetAtespace()
	offset, size, err := pageStart(atespace, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	// One workspace more than the page shows whether another page follows.
	workspaces, err := s.store.ListWorkspaces(ctx, atespace, size+1, offset)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing workspaces: %v", err)
	}
	workspaces, next := pageOf(workspaces, atespace, offset, size)
	return &v1alpha1.ListWorkspacesResponse{Workspaces: workspaces, NextPageToken: next}, nil
}

func (s *Server) CreateWorkspace(ctx context.Context, req *v1alpha1.CreateWorkspaceRequest) (*v1alpha1.Workspace, error) {
	if req == nil || req.Workspace == nil {
		return nil, status.Error(codes.InvalidArgument, "workspace required")
	}
	if err := apivalidation.ValidateWorkspace(req.Workspace); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	prewarmEnabled := s.workspaces.PrewarmEnabled()

	ws := req.Workspace
	ws.Status = nil
	ws.Metadata = defaultMetadata(ws.Metadata, nil)

	atespace, wsName := ws.Metadata.Atespace, ws.Metadata.Name
	unlock, err := s.locker.Lock(ctx, "workspace", atespace, wsName)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "locking workspace %s/%s: %v", atespace, wsName, err)
	}
	defer unlock()

	ws, setupGateway, err := s.saveOrLoadWorkspace(ctx, ws, prewarmEnabled)
	if err != nil || ws.GetStatus().GetPhase() != v1alpha1.WorkspacePhasePreparing {
		return ws, err
	}
	return s.prepareWorkspace(ctx, ws, setupGateway)
}

func (s *Server) lookupSetupGateway(ctx context.Context, atespace string, spec *v1alpha1.WorkspaceSpec) (*v1alpha1.Gateway, error) {
	gwName := spec.GetSetup().GetGateway().GetName()
	if gwName == "" {
		return nil, nil
	}
	gw, err := s.store.GetGateway(ctx, atespace, gwName)
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.FailedPrecondition, "gateway %q not found in atespace %q", gwName, atespace)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "getting gateway %q: %v", gwName, err)
	}
	return gw, nil
}

// saveOrLoadWorkspace handles repeated `ax apply` calls (enforcing immutability)
// and persists a newly created Workspace. A Workspace that still needs pre-warming
// is returned in Phase=Preparing alongside its resolved setup.gateway; an already
// Ready Workspace (or one created while pre-warming is disabled) returns early
// without requiring setup.gateway to still exist.
func (s *Server) saveOrLoadWorkspace(ctx context.Context, ws *v1alpha1.Workspace, prewarmEnabled bool) (*v1alpha1.Workspace, *v1alpha1.Gateway, error) {
	atespace, wsName := ws.Metadata.Atespace, ws.Metadata.Name
	existing, err := s.store.GetWorkspace(ctx, atespace, wsName)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, nil, status.Errorf(codes.Internal, "checking existing workspace: %v", err)
	}
	if err == nil {
		if !workspaceSpecEqual(existing.GetSpec(), ws.GetSpec()) {
			return nil, nil, status.Errorf(codes.FailedPrecondition, "workspace %s/%s already exists and is immutable", atespace, wsName)
		}
		if existing.GetStatus().GetPhase() == v1alpha1.WorkspacePhaseFailed {
			return nil, nil, status.Errorf(codes.FailedPrecondition, "workspace %s/%s failed setup (%s); delete and recreate it", atespace, wsName, existing.GetStatus().GetLastSetupError())
		}
		if existing.GetStatus().GetPhase() != v1alpha1.WorkspacePhasePreparing {
			return existing, nil, nil
		}
	}

	setupGateway, err := s.lookupSetupGateway(ctx, atespace, ws.GetSpec())
	if err != nil {
		return nil, nil, err
	}
	if existing != nil {
		return existing, setupGateway, nil
	}
	if prewarmEnabled {
		ws.Status = &v1alpha1.WorkspaceStatus{Phase: v1alpha1.WorkspacePhasePreparing}
	}
	if err := s.store.SaveWorkspace(ctx, ws); err != nil {
		return nil, nil, status.Errorf(codes.Internal, "saving workspace: %v", err)
	}
	return ws, setupGateway, nil
}

func (s *Server) prepareWorkspace(ctx context.Context, ws *v1alpha1.Workspace, setupGateway *v1alpha1.Gateway) (*v1alpha1.Workspace, error) {
	atespace, wsName := ws.Metadata.Atespace, ws.Metadata.Name
	st, prepErr := s.workspaces.Prepare(ctx, ws, setupGateway)
	ws.Status = st

	// Use an independent context for persisting status so a canceled or timed-out
	// client RPC still records the Preparing/condition state in the store.
	statusCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workspaceStatusUpdateTimeout)
	defer cancel()
	if err := s.store.UpdateWorkspaceStatus(statusCtx, atespace, wsName, st); err != nil {
		return nil, status.Errorf(codes.Internal, "updating workspace status: %v", err)
	}
	if prepErr == nil {
		return ws, nil
	}
	switch {
	case errors.Is(prepErr, context.Canceled) || status.Code(prepErr) == codes.Canceled:
		return nil, status.Errorf(codes.Canceled, "preparing workspace %s/%s: %v", atespace, wsName, prepErr)
	case substrate.IsTransientError(prepErr):
		return nil, status.Errorf(codes.Unavailable, "preparing workspace %s/%s: %v", atespace, wsName, prepErr)
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "preparing workspace %s/%s: %s", atespace, wsName, st.GetLastSetupError())
	}
}

func (s *Server) DeleteWorkspace(ctx context.Context, req *v1alpha1.DeleteWorkspaceRequest) (*emptypb.Empty, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing request")
	}
	atespace := atespaceOrDefault(req.Atespace)
	wsName := req.Name

	// Acquire exclusive lock for this workspace.
	unlock, err := s.locker.Lock(ctx, "workspace", atespace, wsName)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "locking workspace %s/%s: %v", atespace, wsName, err)
	}
	defer unlock()

	ws, err := s.store.GetWorkspace(ctx, atespace, wsName)
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "workspace %q not found in atespace %q", wsName, atespace)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "getting workspace: %v", err)
	}

	if err := s.workspaces.Delete(ctx, atespace, ws.GetMetadata().GetUid()); err != nil {
		return nil, substrateError(err, "deleting workspace resources on substrate")
	}
	if err := s.store.DeleteWorkspace(ctx, atespace, wsName); err != nil {
		return nil, status.Errorf(codes.Internal, "deleting workspace: %v", err)
	}
	return &emptypb.Empty{}, nil
}

func workspaceSpecEqual(a, b *v1alpha1.WorkspaceSpec) bool {
	if a == nil {
		a = &v1alpha1.WorkspaceSpec{}
	}
	if b == nil {
		b = &v1alpha1.WorkspaceSpec{}
	}
	return proto.Equal(a, b)
}
