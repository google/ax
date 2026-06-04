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

// Package harnessconfig provides configuration structures for the AX harness build.
package harnessconfig

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config represents the main configuration for the AX harness server.
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	EventLog  EventLogConfig  `yaml:"eventlog"`
	Harnesses HarnessesConfig `yaml:"harnesses,omitempty"`
	ATE       ATEConfig       `yaml:"ate,omitempty"`
}

// ServerConfig configures the gRPC server.
type ServerConfig struct {
	Address string `yaml:"address"` // Server address to listen on (e.g., ":8494")
}

// SQLiteConfig configures the SQLite event log file.
type SQLiteConfig struct {
	Filename string `yaml:"filename"` // SQLite file for event log storage
}

// EventLogConfig configures the event log storage.
type EventLogConfig struct {
	SQLiteConfig SQLiteConfig `yaml:"sqlite"`
}

// ATEConfig configures the substrate control plane endpoint used by
// substrate harnesses.
type ATEConfig struct {
	Endpoint string `yaml:"endpoint"`
}

// HarnessesConfig groups harnesses to serve by type. Each type maps to a list
// of that type's configurations.
type HarnessesConfig struct {
	// Default is the id of the harness to serve when a request specifies no harness.
	Default string `yaml:"default,omitempty"`
	// Antigravity harnesses connect to a gRPC server at a fixed address.
	Antigravity []AntigravityHarnessConfig `yaml:"antigravity,omitempty"`
	// Substrate harnesses are brought up as SubstrATE actors from an ActorTemplate.
	Substrate []SubstrateHarnessConfig `yaml:"substrate,omitempty"`
}

// AntigravityHarnessConfig registers an Antigravity harness, which connects to
// a gRPC server at a fixed address.
type AntigravityHarnessConfig struct {
	ID      string `yaml:"id"`      // Unique harness identifier
	Address string `yaml:"address"` // gRPC address of the harness server
}

// SubstrateHarnessConfig registers a harness backed by a SubstrATE ActorTemplate.
type SubstrateHarnessConfig struct {
	ID        string `yaml:"id"`             // Unique harness identifier
	Namespace string `yaml:"namespace"`      // ActorTemplate namespace
	Template  string `yaml:"template"`       // ActorTemplate name
	Port      int    `yaml:"port,omitempty"` // HarnessService port (default 50053)
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
	if c.EventLog.SQLiteConfig.Filename == "" {
		return fmt.Errorf("eventlog.sqlite.filename is required")
	}

	return nil
}
