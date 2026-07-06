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

// Package config provides configuration for the controller server path.
package config

import (
	"fmt"
	"os"

	"github.com/google/ax/internal/harness"
	"github.com/google/ax/internal/harness/substrate"
	"gopkg.in/yaml.v3"
)

const (
	// The substrate namespace reserved for AX's built-in harnesses.
	defaultNamespace = "ax"
	// The port for harnesses running as substrate actors. Substrate's
	// actor networking DNATs inbound workerPodIP:80 to the actor.
	substrateDefaultPort = 80
)

// Config represents the main configuration for the AX harness server.
type Config struct {
	Version   string          `yaml:"version"`
	Server    ServerConfig    `yaml:"server"`
	EventLog  EventLogConfig  `yaml:"eventlog"`
	Harnesses HarnessesConfig `yaml:"harnesses,omitempty"`
	Telemetry TelemetryConfig `yaml:"telemetry,omitempty"`
}

// ServerConfig configures the gRPC server.
type ServerConfig struct {
	Address string `yaml:"address"` // Server address to listen on (e.g., ":8494")
}

// TelemetryConfig configures telemetry options.
type TelemetryConfig struct {
	OTLP OTLPConfig `yaml:"otlp,omitempty"`
}

// OTLPConfig configures the OTLP exporter.
type OTLPConfig struct {
	Enabled  bool   `yaml:"enabled,omitempty"`
	Endpoint string `yaml:"endpoint,omitempty"` // OTLP collector endpoint (e.g., "localhost:4317")
}

// SQLiteConfig configures the SQLite event log file.
type SQLiteConfig struct {
	Filename string `yaml:"filename"` // SQLite file for event log storage
}

// PostgresConfig configures the Postgres event log.
type PostgresConfig struct {
	DSN string `yaml:"dsn"` // Postgres connection DSN
}

// EventLogConfig configures the event log storage.
type EventLogConfig struct {
	SQLiteConfig   SQLiteConfig   `yaml:"sqlite,omitempty"`
	PostgresConfig PostgresConfig `yaml:"postgres,omitempty"`
}

// HarnessesConfig groups harnesses to serve by type. There are two categories:
//   - Built-in harnesses (e.g. Antigravity) whose implementation and container
//     image are provided by AX.
//   - Custom harnesses on substrate whose implementation and container image are
//     provided by the user via their own ActorTemplate.
type HarnessesConfig struct {
	Antigravity AntigravityHarnessConfig `yaml:"antigravity,omitempty"`
	Substrate   []SubstrateHarnessConfig `yaml:"substrate,omitempty"`
}

// AntigravityHarnessConfig registers the built-in Antigravity harness.
type AntigravityHarnessConfig struct {
	Default  bool   `yaml:"default,omitempty"`
	Endpoint string `yaml:"endpoint,omitempty"` // HarnessService address
}

// SubstrateHarnessConfig registers a custom harness deployed on substrate
// from a user-provided container image.
type SubstrateHarnessConfig struct {
	ID        string `yaml:"id"`                // Unique harness identifier
	Namespace string `yaml:"namespace"`         // ActorTemplate namespace (user-owned, not "ax")
	Template  string `yaml:"template"`          // ActorTemplate name
	Port      int    `yaml:"port,omitempty"`    // HarnessService port
	Default   bool   `yaml:"default,omitempty"` // Default harness or not
}

// NewHarness builds the custom harness. Custom harnesses always run as substrate
// actors from the user's own ActorTemplate.
func (c SubstrateHarnessConfig) NewHarness(endpoint string) (harness.Harness, error) {
	port := c.Port
	if port == 0 {
		port = substrateDefaultPort
	}
	return newSubstrateHarness(c.ID, endpoint, c.Namespace, c.Template, port)
}

// newSubstrateHarness brings up a harness that is deployed as a substrate actor.
func newSubstrateHarness(harnessID, endpoint, namespace, template string, port int) (harness.Harness, error) {
	sh, err := substrate.New(harnessID, endpoint, namespace, template, port)
	if err != nil {
		return nil, err
	}
	return sh, nil
}

// LoadFromFile loads configuration from a YAML file.
func LoadFromFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	cfg.setDefaults()

	return &cfg, nil
}

// DefaultConfig returns a configuration with default values set.
func DefaultConfig() *Config {
	var cfg Config
	cfg.setDefaults()
	return &cfg
}

// setDefaults sets default values for optional fields.
func (c *Config) setDefaults() {
	if c.Server.Address == "" {
		c.Server.Address = ":8494"
	}
	if c.EventLog.SQLiteConfig.Filename == "" {
		c.EventLog.SQLiteConfig.Filename = "eventlog/log.sqlite"
	}
}

// Validate validates the configuration.
func (c *Config) Validate() error {
	if c.Server.Address == "" {
		return fmt.Errorf("server.address is required")
	}
	if c.EventLog.PostgresConfig.DSN == "" && c.EventLog.SQLiteConfig.Filename == "" {
		return fmt.Errorf("eventlog requires either postgres.dsn or sqlite.filename")
	}

	var defaultCount int
	if c.Harnesses.Antigravity.Default {
		defaultCount++
	}

	for _, sc := range c.Harnesses.Substrate {
		if sc.ID == "" {
			return fmt.Errorf("substrate harness id is required")
		}
		if sc.ID == "antigravity" {
			return fmt.Errorf("substrate harness id %q is reserved for the built-in antigravity harness", sc.ID)
		}
		if sc.Namespace == "" {
			return fmt.Errorf("substrate harness %q: namespace is required", sc.ID)
		}
		if sc.Namespace == defaultNamespace {
			return fmt.Errorf("substrate harness %q: namespace %q is reserved for built-in harnesses", sc.ID, defaultNamespace)
		}
		if sc.Template == "" {
			return fmt.Errorf("substrate harness %q: template is required", sc.ID)
		}
		if sc.Default {
			defaultCount++
		}
	}

	if defaultCount > 1 {
		return fmt.Errorf("multiple harnesses marked as default")
	}

	return nil
}
