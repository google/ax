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

package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/google/ax/internal/agent"
	"github.com/google/ax/internal/config"
	"github.com/google/ax/internal/experimental/a2abridge"
	expagent "github.com/google/ax/internal/experimental/agent"
)

// Registry manages a collection of local and remote agents.
// It provides agent discovery, health monitoring, and load balancing.
type Registry struct {
	mu        sync.RWMutex
	agents    map[string]agent.Agent
	agentInfo map[string]*agent.AgentInfo
}

// NewRegistry creates a new agent registry.
func NewRegistry() *Registry {
	return &Registry{
		agents:    make(map[string]agent.Agent),
		agentInfo: make(map[string]*agent.AgentInfo),
	}
}

func (r *Registry) Map() map[string]agent.Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.agents
}

// RegisterLocal registers a local (in-process) agent.
func (r *Registry) RegisterLocal(cfg config.LocalAgentConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := validateID(cfg.ID); err != nil {
		return err
	}

	if _, ok := r.agents[cfg.ID]; ok {
		return fmt.Errorf("agent %s already registered", cfg.ID)
	}

	r.agents[cfg.ID] = cfg.Agent
	r.agentInfo[cfg.ID] = &agent.AgentInfo{
		ID:          cfg.ID,
		Name:        cfg.Name,
		Description: cfg.Description,
		Metadata:    cfg.Metadata,
	}

	return nil
}

// RegisterRemote registers a remote agent by creating a remote agent client.
// The protocol field determines what kind of remote agent to register
// (matched case-insensitively):
//   - "axp" (default): AX's proto.AgentService.
//   - "a2a":           A2A protocol.
func (r *Registry) RegisterRemote(ctx context.Context, cfg config.RemoteAgentConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := validateID(cfg.ID); err != nil {
		return err
	}
	if _, ok := r.agents[cfg.ID]; ok {
		return fmt.Errorf("agent %s already registered", cfg.ID)
	}

	switch strings.ToLower(cfg.Protocol) {
	case "", "axp":
		return r.registerRemote(cfg)
	case "a2a":
		return r.registerA2A(ctx, cfg)
	default:
		return fmt.Errorf("remote agent %s: invalid protocol %q (want \"axp\" or \"a2a\")", cfg.ID, cfg.Protocol)
	}
}

func (r *Registry) registerRemote(cfg config.RemoteAgentConfig) error {
	remoteAgent, err := agent.NewRemoteAgent(agent.RemoteAgentConfig{
		Address:    cfg.Address,
		Reconnect:  true,
		MaxRetries: 3,
	})
	if err != nil {
		return fmt.Errorf("failed to create remote agent: %w", err)
	}
	r.agents[cfg.ID] = remoteAgent
	r.agentInfo[cfg.ID] = &agent.AgentInfo{
		ID:          cfg.ID,
		Name:        cfg.Name,
		Description: cfg.Description,
		Metadata:    cfg.Metadata,
	}
	return nil
}

// Creates an A2A-protocol agent client. The agent's AgentCard is resolved at
// registration time and used to populate the agent's information.
func (r *Registry) registerA2A(ctx context.Context, cfg config.RemoteAgentConfig) error {
	a2aAgent, err := expagent.NewA2AAgent(ctx, expagent.A2AAgentConfig{
		ID:        cfg.ID,
		Address:   cfg.Address,
		Auth:      cfg.Auth,
		Headers:   cfg.Headers,
		Stateless: cfg.A2A.Stateless,
	})
	if err != nil {
		return fmt.Errorf("failed to create a2a agent: %w", err)
	}
	name, description := a2abridge.AgentMetadataFromCard(a2aAgent.Card(), cfg.Name, cfg.Description)
	r.agents[cfg.ID] = a2aAgent
	r.agentInfo[cfg.ID] = &agent.AgentInfo{
		ID:          cfg.ID,
		Name:        name,
		Description: description,
		Metadata:    cfg.Metadata,
	}
	return nil
}


// Get retrieves an agent by ID.
func (r *Registry) Get(id string) (agent.Agent, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	a, exists := r.agents[id]
	if !exists {
		return nil, fmt.Errorf("agent %s not found", id)
	}

	return a, nil
}

// AgentInfo retrieves agent metadata by ID.
func (r *Registry) AgentInfo(id string) (*agent.AgentInfo, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	info, ok := r.agentInfo[id]
	if !ok {
		return nil, fmt.Errorf("agent %s not found", id)
	}

	return info, nil
}

// List returns all registered agent IDs.
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := make([]string, 0, len(r.agents))
	for id := range r.agents {
		ids = append(ids, id)
	}

	return ids
}

// Close stops the registry and closes all agents.
func (r *Registry) Close() error {

	r.mu.Lock()
	defer r.mu.Unlock()

	// Close all agents
	var firstErr error
	for id, a := range r.agents {
		if err := a.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("failed to close agent %s: %w", id, err)
		}
	}

	return firstErr
}
