//go:build !harness

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

package cliutil

import (
	"context"
	"fmt"
	"time"

	"github.com/google/ax/internal/agent"
	"github.com/google/ax/internal/config"
	"github.com/google/ax/internal/controller"
	"github.com/google/ax/internal/controller/executor"
	"github.com/google/ax/internal/gemini"
)

// Controller is the active controller type for this build.
type Controller = *controller.Controller

// ExecHandler is the handler type accepted by Controller.Exec.
type ExecHandler = controller.ExecHandler

// Config is the configuration type for this build.
type Config = config.Config

// LoadFromFile loads configuration from a YAML file.
func LoadFromFile(path string) (*Config, error) {
	return config.LoadFromFile(path)
}

// DefaultConfig returns a configuration with default values set.
func DefaultConfig() *Config {
	return config.DefaultConfig()
}

// NewControllerFromConfig creates a new Controller instance based on the provided configuration.
func NewControllerFromConfig(ctx context.Context, cfg *Config) (*controller.Controller, error) {
	// Validate planner type early
	switch cfg.Planner.Type {
	case "gemini":
		// valid
	default:
		return nil, fmt.Errorf("unknown planner type: %s", cfg.Planner.Type)
	}

	// Create event log builder
	eventLogBuilder := func() (executor.EventLog, error) {
		return executor.OpenSQLiteEventLog(cfg.EventLog.SQLiteConfig.Filename)
	}

	// Create planner builder
	plannerBuilder := func(ctx context.Context, r *controller.Registry) (agent.Agent, error) {
		switch cfg.Planner.Type {
		case "gemini":
			timeout, err := time.ParseDuration(cfg.Planner.Gemini.Timeout)
			if err != nil {
				return nil, fmt.Errorf("failed to parse duration: %v", err)
			}
			return gemini.NewGeminiPlannerAgent(ctx, r, gemini.GeminiPlannerConfig{
				GeminiConfig: &config.GeminiConfig{
					Model:        cfg.Planner.Gemini.Model,
					MaxTokens:    cfg.Planner.Gemini.MaxTokens,
					Temperature:  cfg.Planner.Gemini.Temperature,
					Timeout:      timeout,
					SystemPrompt: cfg.Planner.Gemini.SystemPrompt,
				},
				SkillsDir: cfg.Planner.Gemini.SkillsDir,
			})
		default:
			return nil, fmt.Errorf("unknown planner type: %s", cfg.Planner.Type)
		}
	}

	// Build controller config
	controllerConfig := controller.Config{
		EventLogBuilder: eventLogBuilder,
		PlannerBuilder:  plannerBuilder,
	}

	// Create controller
	c, err := controller.New(ctx, controllerConfig)
	if err != nil {
		return nil, err
	}

	for _, agentCfg := range cfg.Registry.RemoteAgents {
		if err := c.Registry().RegisterRemote(ctx, agentCfg); err != nil {
			return nil, fmt.Errorf("failed to register remote agent %s: %w", agentCfg.ID, err)
		}
	}


	return c, nil
}
