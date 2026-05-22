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

package agent

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/ax/internal/agent"
	"github.com/google/ax/internal/auth"
	"github.com/google/ax/internal/experimental/k8s/ate"
	"github.com/google/ax/proto"
)

// SubstrateAgent manages execution in a SubstrATE actor.
type SubstrateAgent struct {
	ateClient *ate.Client
	config    SubstrateAgentConfig
}

// SubstrateAgentConfig configures an ATE agent client.
type SubstrateAgentConfig struct {
	ID        string
	Namespace string
	Template  string
	Port      int // Port where agent runs in the worker
	Protocol  string
	Auth      auth.Auth
	Headers   auth.Headers
}

// NewSubstrateAgent creates a new ATE agent client.
func NewSubstrateAgent(endpoint string, config SubstrateAgentConfig) (*SubstrateAgent, error) {
	if config.Port == 0 {
		config.Port = 8494 // Default port
	}
	client, err := ate.NewClient(config.Namespace, config.Template, endpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to create ATE client: %w", err)
	}
	return &SubstrateAgent{
		ateClient: client,
		config:    config,
	}, nil
}

// Connect handles processing of input content by creating an actor and delegating to RemoteAgent.
func (a *SubstrateAgent) Connect(ctx context.Context, conversationID string, execID string, start *proto.AgentStart, e agent.Executor, o agent.OutputHandler) error {
	// 1. Create Actor
	resp, err := a.ateClient.CreateActor(ctx, execID)
	if err != nil {
		return fmt.Errorf("failed to create actor: %w", err)
	}
	actor := resp.Actor
	if actor == nil {
		return fmt.Errorf("received nil actor in response")
	}
	if actor.AteomPodIp == "" {
		return fmt.Errorf("actor has no active worker IP address (AteomPodIp is empty)")
	}

	workerAddr := fmt.Sprintf("%s:%d", actor.AteomPodIp, a.config.Port)
	// 2. Connect to the Actor.
	var activeAgent agent.Agent
	switch strings.ToLower(a.config.Protocol) {
	case "", "axp":
		activeAgent, err = agent.NewRemoteAgent(agent.RemoteAgentConfig{
			Address:    workerAddr,
			Reconnect:  true,
			MaxRetries: 3,
		})
	case "a2a":
		activeAgent, err = NewA2AAgent(ctx, A2AAgentConfig{
			ID:                a.config.ID,
			Address:           workerAddr,
			Auth:              a.config.Auth,
			Headers:           a.config.Headers,
			Stateless:         true,
			OverrideCardHosts: true,
		})
	default:
		return fmt.Errorf("ate agent %s: invalid protocol %q", a.config.ID, a.config.Protocol)
	}
	if err != nil {
		return fmt.Errorf("failed to create remote agent connection: %w", err)
	}
	defer activeAgent.Close()

	// 3. Suspend Actor when done.
	defer func() {
		// TODO(jbd): Actors need to be deleted when they are done.
		// DeleteActor once deletion is possible. For now, suspending allows
		// us to return the worker back to the pool.
		log.Printf("Suspending ATE actor for execution %s", execID)
		suspendCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := a.ateClient.SuspendActor(suspendCtx, execID); err != nil {
			log.Printf("Failed to suspend actor %s: %v", execID, err)
		}
	}()

	return activeAgent.Connect(ctx, conversationID, execID, start, e, o)
}

// Close gracefully shuts down the ATE agent connection.
func (a *SubstrateAgent) Close() error {
	if a.ateClient != nil {
		return a.ateClient.Close()
	}
	return nil
}
