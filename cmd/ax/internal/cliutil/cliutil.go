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
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/google/ax/internal/config"
	"github.com/google/ax/internal/controller"
	"github.com/google/ax/internal/controller/eventlog"
	"github.com/google/ax/internal/harness"
	"github.com/google/ax/internal/harness/antigravity"
	"github.com/google/ax/internal/harness/substrate"
)

const antigravityHarnessID = "antigravity"

// defaultAntigravityAddress is the loopback address on which the Antigravity
// sidecar listens by default. Kept here (rather than only inside
// antigravity.New) so autoStartAntigravitySidecar can parse host/port before
// constructing the harness.
const defaultAntigravityAddress = "127.0.0.1:50053"

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

	// Built-in harnesses.
	var defaultHarnessID string
	var antigravityHarness harness.Harness
	var err error
	if !substrateMode {
		address := cfg.Harnesses.Antigravity.Endpoint
		if address == "" {
			address = defaultAntigravityAddress
		}
		ah := antigravity.New(address)
		// Fork (or reuse) the Antigravity Python sidecar for the local execution
		// path. Skipped when the user explicitly points at a non-loopback endpoint
		// (they are managing the sidecar out-of-band) or when
		// AX_ANTIGRAVITY_NO_AUTOSTART=1 is set (used by tests and by users who
		// want to run ax harness in a separate terminal without any probing).
		if err := autoStartAntigravitySidecar(ctx, ah, address); err != nil {
			return nil, fmt.Errorf("antigravity harness sidecar: %w", err)
		}
		antigravityHarness = ah
	} else {
		antigravityHarness, err = substrate.New(antigravityHarnessID, "", "", "", 80)
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

// autoStartAntigravitySidecar probes the configured harness address; if nothing
// is serving there, it forks the Antigravity Python sidecar and waits for it to
// become healthy. The sidecar's lifetime is bound to the harness (torn down via
// controller.Registry.Close). See antigravity.EnsureSidecar for the reuse-vs-fork
// logic.
//
// Environment overrides:
//   - AX_ANTIGRAVITY_NO_AUTOSTART=1 disables autostart entirely (the user is
//     responsible for starting ax harness or an equivalent sidecar).
//   - AX_ANTIGRAVITY_SIDECAR_CMD overrides the argv used to fork the sidecar.
//     Split on whitespace; --host and --port are appended automatically.
//     Useful for pointing at a venv's python (e.g.
//     "/path/to/venv/bin/python3 -m python.antigravity.harness_server").
//
// Skipped when the address is non-loopback (user manages the sidecar
// out-of-band).
func autoStartAntigravitySidecar(ctx context.Context, h *antigravity.AntigravityHarness, address string) error {
	if os.Getenv("AX_ANTIGRAVITY_NO_AUTOSTART") == "1" {
		return nil
	}
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		// Malformed address: fall back to leaving the sidecar unmanaged so
		// the user gets the original clear "connection refused" error at
		// dial time rather than a cryptic autostart-parse failure here.
		return nil
	}
	if !isLoopback(host) {
		return nil
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil
	}
	cfg := antigravity.SidecarConfig{Host: host, Port: port}
	if cmdOverride := strings.Fields(os.Getenv("AX_ANTIGRAVITY_SIDECAR_CMD")); len(cmdOverride) > 0 {
		cfg.Command = cmdOverride
	}
	sc, err := antigravity.EnsureSidecar(ctx, cfg)
	if err != nil {
		return err
	}
	h.AttachSidecar(sc)
	return nil
}

// isLoopback reports whether host is a well-known loopback name or a loopback
// IP literal. We only autostart the sidecar for loopback addresses because a
// non-loopback endpoint implies the user is pointing at a sidecar they run
// themselves (e.g. in a container, on a remote host).
func isLoopback(host string) bool {
	switch host {
	case "localhost", "":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
