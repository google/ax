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

func (s *Server) CreateGateway(ctx context.Context, req *v1alpha1.CreateGatewayRequest) (*v1alpha1.Gateway, error) {
	if req == nil || req.Gateway == nil {
		return nil, status.Error(codes.InvalidArgument, "gateway required")
	}
	return s.saveGateway(ctx, req.Gateway, true)
}

func (s *Server) UpdateGateway(ctx context.Context, req *v1alpha1.UpdateGatewayRequest) (*v1alpha1.Gateway, error) {
	if req == nil || req.Gateway == nil {
		return nil, status.Error(codes.InvalidArgument, "gateway required")
	}
	return s.saveGateway(ctx, req.Gateway, false)
}

// saveGateway validates and saves gw. The gateway must be new when create is
// set, and must exist otherwise.
func (s *Server) saveGateway(ctx context.Context, gw *v1alpha1.Gateway, create bool) (*v1alpha1.Gateway, error) {
	if err := apivalidation.ValidateGateway(gw); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	atespace := gw.Metadata.Atespace
	if atespace == "" {
		atespace = "default"
	}
	gwName := gw.Metadata.Name

	// Acquire exclusive lock for this gateway
	unlock, err := s.locker.Lock(ctx, "gateway", atespace, gwName)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "locking gateway %s/%s: %v", atespace, gwName, err)
	}
	defer unlock()

	existing, err := s.store.GetGateway(ctx, atespace, gwName)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.Internal, "getting gateway: %v", err)
	}
	exists := err == nil
	if create && exists {
		return nil, status.Errorf(codes.AlreadyExists, "gateway %q already exists in atespace %q", gwName, atespace)
	}
	if !create && !exists {
		return nil, status.Errorf(codes.NotFound, "gateway %q not found in atespace %q", gwName, atespace)
	}
	gw.Metadata = defaultMetadata(gw.Metadata, func(string, string) *v1alpha1.ObjectMeta {
		return existing.GetMetadata()
	})

	if err := s.store.SaveGateway(ctx, gw); err != nil {
		return nil, status.Errorf(codes.Internal, "saving gateway: %v", err)
	}
	return gw, nil
}

func (s *Server) DeleteGateway(ctx context.Context, req *v1alpha1.DeleteGatewayRequest) (*v1alpha1.DeleteGatewayResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing request")
	}
	atespace := req.Atespace
	if atespace == "" {
		atespace = "default"
	}
	gwName := req.Name

	// Acquire exclusive lock for this gateway
	unlock, err := s.locker.Lock(ctx, "gateway", atespace, gwName)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "locking gateway %s/%s: %v", atespace, gwName, err)
	}
	defer unlock()

	if err := s.store.DeleteGateway(ctx, atespace, gwName); err != nil {
		return nil, status.Errorf(codes.Internal, "deleting gateway: %v", err)
	}
	return &v1alpha1.DeleteGatewayResponse{}, nil
}
