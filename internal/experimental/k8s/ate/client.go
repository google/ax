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

// Package ate provides a client for the Agent Substrate Control API.
package ate

import (
	"context"
	"fmt"

	"github.com/agent-substrate/substrate/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Client struct {
	namespace string
	template  string
	conn      *grpc.ClientConn
}

// NewClient creates a new actor client.
func NewClient(ns, template, target string, opts ...grpc.DialOption) (*Client, error) {
	if ns == "" {
		return nil, fmt.Errorf("namespace cannot be empty")
	}
	if template == "" {
		return nil, fmt.Errorf("template cannot be empty")
	}
	if target == "" {
		target = "api.ate-system.svc:443"
	}
	if len(opts) == 0 {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, fmt.Errorf("error when creating Control client: %w", err)
	}
	return &Client{
		namespace: ns,
		template:  template,
		conn:      conn,
	}, nil
}

// CreateActor creates a new actor.
func (c *Client) CreateActor(ctx context.Context, id string) (*ateapipb.CreateActorResponse, error) {
	client := ateapipb.NewControlClient(c.conn)
	resp, err := client.CreateActor(ctx, &ateapipb.CreateActorRequest{
		ActorId:                id,
		ActorTemplateNamespace: c.namespace,
		ActorTemplateName:      c.template,
	})
	if err != nil {
		return nil, fmt.Errorf("error when calling Control.CreateActor: %w", err)
	}
	return resp, nil
}

// SuspendActor suspends the actor.
func (c *Client) SuspendActor(ctx context.Context, id string) (*ateapipb.SuspendActorResponse, error) {
	client := ateapipb.NewControlClient(c.conn)
	resp, err := client.SuspendActor(ctx, &ateapipb.SuspendActorRequest{
		ActorId: id,
	})
	if err != nil {
		return nil, fmt.Errorf("error when calling Control.SuspendActor: %w", err)
	}
	return resp, nil
}

// Close closes the gRPC connection.
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}
