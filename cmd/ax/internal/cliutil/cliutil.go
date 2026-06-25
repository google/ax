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

	"github.com/google/ax/internal/config2"
	"github.com/google/ax/internal/controller2"
	"github.com/google/ax/internal/controller2/eventlog"
	"github.com/google/ax/internal/harness"
)

const antigravityHarnessID = "antigravity"

// Controller is the active controller type for this build.
type Controller = *controller2.Controller

// ExecHandler is the handler type accepted by Controller.Exec.
type ExecHandler = controller2.ExecHandler

// Config is the configuration type for this build.
type Config = config2.Config

// LoadFromFile loads configuration from a YAML file.
func LoadFromFile(path string) (*Config, error) {
	return config2.LoadFromFile(path)
}

// DefaultConfig returns a configuration with default values set.
func DefaultConfig() *Config {
	return config2.DefaultConfig()
}

// NewControllerFromConfig creates a controller2.Controller instance based on the provided configuration.
func NewControllerFromConfig(ctx context.Context, cfg *Config) (*controller2.Controller, error) {
	reg := controller2.NewRegistry()

	// AX_SUBSTRATE selects how built-in harnesses run: locally (unset) or as
	// substrate actors ("1").
	substrateMode := os.Getenv("AX_SUBSTRATE") == "1"

	// Built-in harnesses.
	var defaultHarnessID string
	var antigravityHarness harness.Harness
	var err error
	if !substrateMode {
		address := cfg.Harnesses.Antigravity.Endpoint
		if address == "" {
			address = "127.0.0.1:50053"
		}
		antigravityHarness = harness.NewAntigravityHarness(address)
	} else {
		antigravityHarness, err = harness.NewSubstrateHarness(antigravityHarnessID, "", "", "", 80)
		if err != nil {
			return nil, fmt.Errorf("antigravity harness: %w", err)
		}
	}
	if err := reg.RegisterHarness(antigravityHarnessID, antigravityHarness); err != nil {
		return nil, fmt.Errorf("register antigravity harness: %w", err)
	}
	if cfg.Harnesses.Antigravity.Default {
		defaultHarnessID = antigravityHarnessID
	}

	// Custom substrate harnesses.
	if len(cfg.Harnesses.Substrate) > 0 && !substrateMode {
		return nil, fmt.Errorf("custom substrate harnesses require AX_SUBSTRATE=1")
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

	return controller2.New(ctx, controller2.Config{
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
