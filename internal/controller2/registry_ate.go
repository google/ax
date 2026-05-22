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

package controller2

import (
	"context"
	"fmt"

	"github.com/google/ax/internal/agent"
	"github.com/google/ax/internal/config"
	expagent "github.com/google/ax/internal/experimental/agent"
)

// RegisterATE registers an ATE agent by creating an ATE agent client.
func (r *Registry) RegisterATE(ctx context.Context, ateTarget string, cfg config.SubstrateAgentConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := validateID(cfg.ID); err != nil {
		return err
	}

	if _, ok := r.agents[cfg.ID]; ok {
		return fmt.Errorf("agent %s already registered", cfg.ID)
	}

	// Create ATE agent client
	substrateAgent, err := expagent.NewSubstrateAgent(ateTarget, expagent.SubstrateAgentConfig{
		ID:        cfg.ID,
		Namespace: cfg.Namespace,
		Template:  cfg.Template,
		Port:      cfg.Port,
		Protocol:  cfg.Protocol,
		Auth:      cfg.Auth,
		Headers:   cfg.Headers,
	})
	if err != nil {
		return fmt.Errorf("failed to create ATE agent: %w", err)
	}

	r.agents[cfg.ID] = substrateAgent
	r.agentInfo[cfg.ID] = &agent.AgentInfo{
		ID:          cfg.ID,
		Name:        cfg.Name,
		Description: cfg.Description,
		Metadata:    cfg.Metadata,
	}

	return nil
}
