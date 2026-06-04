//go:build harness

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

	"github.com/google/ax/internal/config/harnessconfig"
	"github.com/google/ax/internal/controller/executor"
	"github.com/google/ax/internal/controller2"
	"github.com/google/ax/internal/harness"
)

// Controller is the active controller type for this build.
type Controller = *controller2.Controller

// ExecHandler is the handler type accepted by Controller.Exec.
type ExecHandler = controller2.ExecHandler

// Config is the configuration type for this build.
type Config = harnessconfig.Config

// LoadFromFile loads configuration from a YAML file.
func LoadFromFile(path string) (*Config, error) {
	return harnessconfig.LoadFromFile(path)
}

// DefaultConfig returns a configuration with default values set.
func DefaultConfig() *Config {
	return harnessconfig.DefaultConfig()
}

// NewControllerFromConfig creates a controller2.Controller instance based on the provided configuration.
func NewControllerFromConfig(ctx context.Context, cfg *Config) (*controller2.Controller, error) {
	reg := controller2.NewRegistry()

	// Antigravity harnesses.
	for _, hc := range cfg.Harnesses.Antigravity {
		h := harness.NewAntigravityHarness(hc.Address)
		if err := reg.RegisterHarness(hc.ID, h); err != nil {
			return nil, fmt.Errorf("register antigravity harness %q: %w", hc.ID, err)
		}
	}

	// Substrate harnesses.
	for _, sc := range cfg.Harnesses.Substrate {
		sh, err := harness.NewSubstrateHarness(cfg.ATE.Endpoint, sc.Namespace, sc.Template, sc.Port)
		if err != nil {
			return nil, fmt.Errorf("substrate harness %q: %w", sc.ID, err)
		}
		if err := reg.RegisterHarness(sc.ID, sh); err != nil {
			return nil, fmt.Errorf("register substrate harness %q: %w", sc.ID, err)
		}
	}

	// Register the configured default harness.
	if id := cfg.Harnesses.Default; id != "" {
		h, err := reg.Harness(id)
		if err != nil {
			return nil, fmt.Errorf("default harness %q not found", id)
		}
		if err := reg.RegisterHarness("", h); err != nil {
			return nil, fmt.Errorf("register default harness %q: %w", id, err)
		}
	}

	return controller2.New(ctx, controller2.Config{
		Registry: reg,
		EventLogBuilder: func() (executor.EventLog, error) {
			return executor.OpenSQLiteEventLog(cfg.EventLog.SQLiteConfig.Filename)
		},
	})
}
