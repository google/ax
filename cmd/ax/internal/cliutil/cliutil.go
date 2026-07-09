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
	"os"

	"github.com/google/ax/internal/config"
	"github.com/google/ax/internal/controller"
	"github.com/google/ax/internal/controller/eventlog"
	"github.com/google/ax/internal/harness"
	"github.com/google/ax/internal/harness/antigravity"
	"github.com/google/ax/internal/harness/antigravityinteractions"
	"github.com/google/ax/internal/harness/substrate"
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

// NewControllerFromConfig creates a controller.Controller instance based on the provided configuration.
func NewControllerFromConfig(ctx context.Context, cfg *Config) (*controller.Controller, error) {
	reg := controller.NewRegistry()

	// AX_SUBSTRATE selects how built-in harnesses run: locally (unset) or as
	// substrate actors ("1").
	substrateMode := os.Getenv("AX_SUBSTRATE") == "1"

	// Validate config before local-mode antigravity.New forks the Python
	// sidecar, so a config error surfaces without spawning a subprocess.
	if len(cfg.Harnesses.Substrate) > 0 && !substrateMode {
		return nil, fmt.Errorf("custom substrate harnesses require AX_SUBSTRATE=1")
	}

	var defaultHarnessID string
	var err error

	// Built-in Antigravity harness.
	var antigravityHarness harness.Harness
	if !substrateMode {
		address := cfg.Harnesses.Antigravity.Endpoint
		if address == "" {
			address = "127.0.0.1:50053"
		}
		// Local mode: the harness owns the Python sidecar.
		antigravityHarness, err = antigravity.New(ctx, address, true)
		if err != nil {
			return nil, fmt.Errorf("antigravity harness: %w", err)
		}
	} else {
		antigravityHarness, err = substrate.New(config.AntigravityHarnessID, "", "", "", 80)
		if err != nil {
			return nil, fmt.Errorf("antigravity harness: %w", err)
		}
	}
	if err := reg.RegisterHarness(config.AntigravityHarnessID, antigravityHarness); err != nil {
		return nil, fmt.Errorf("register antigravity harness: %w", err)
	}
	if cfg.Harnesses.Antigravity.Default {
		defaultHarnessID = config.AntigravityHarnessID
	}

	// Built-in Antigravity Interactions harness.
	var antigravityInteractionsHarness harness.Harness
	if !substrateMode {
		agent := cfg.Harnesses.AntigravityInteractions.Agent
		if agent == "" {
			agent = antigravityinteractions.DefaultAgent
		}
		stateDir := cfg.Harnesses.AntigravityInteractions.StateDir
		if stateDir == "" {
			stateDir, err = antigravityinteractions.DefaultStateDir()
			if err != nil {
				return nil, fmt.Errorf("antigravity-interactions harness: %w", err)
			}
		}
		antigravityInteractionsHarness, err = antigravityinteractions.New(antigravityinteractions.AntigravityInteractionsConfig{
			Agent:    agent,
			StateDir: stateDir,
		})
	} else {
		antigravityInteractionsHarness, err = substrate.New(config.AntigravityInteractionsHarnessID, "", "", "", 80)
	}
	if err != nil {
		return nil, fmt.Errorf("antigravity-interactions harness: %w", err)
	}
	if err := reg.RegisterHarness(config.AntigravityInteractionsHarnessID, antigravityInteractionsHarness); err != nil {
		return nil, fmt.Errorf("register antigravity-interactions harness: %w", err)
	}
	if cfg.Harnesses.AntigravityInteractions.Default {
		defaultHarnessID = config.AntigravityInteractionsHarnessID
	}

	for _, sc := range cfg.Harnesses.Substrate {
		h, err := sc.NewHarness("")
		if err != nil {
			return nil, fmt.Errorf("substrate harness %q: %w", sc.ID, err)
		}
		if err := reg.RegisterHarness(sc.ID, h); err != nil {
			return nil, fmt.Errorf("register substrate harness %q: %w", sc.ID, err)
		}
		if sc.Default {
			defaultHarnessID = sc.ID
		}
	}

	// Register the configured default harness.
	if defaultHarnessID != "" {
		h, err := reg.Harness(defaultHarnessID)
		if err != nil {
			return nil, fmt.Errorf("default harness %q not found", defaultHarnessID)
		}
		if err := reg.RegisterHarness("", h); err != nil {
			return nil, fmt.Errorf("register default harness %q: %w", defaultHarnessID, err)
		}
	}

	return controller.New(ctx, controller.Config{
		Registry: reg,
		EventLogBuilder: func() (eventlog.EventLog, error) {
			if cfg.EventLog.PostgresConfig.DSN != "" {
				dsn := os.ExpandEnv(cfg.EventLog.PostgresConfig.DSN)
				if dsn == "" {
					return nil, fmt.Errorf("eventlog: postgres dsn %q expanded to empty", cfg.EventLog.PostgresConfig.DSN)
				}
				return eventlog.OpenPostgresEventLog(dsn)
			}
			return eventlog.OpenSQLiteEventLog(cfg.EventLog.SQLiteConfig.Filename)
		},
	})
}
