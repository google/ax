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
	"path/filepath"

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
	// Harness IDs reserved for AX's built-in harnesses.
	AntigravityHarnessID             = "antigravity"
	AntigravityInteractionsHarnessID = "antigravity-interactions"
	// AntigravityHarnessTemplate is the substrate ActorTemplate that runs the
	// Antigravity harness.
	AntigravityHarnessTemplate = "ax-harness-antigravity-template"
	// AntigravityInteractionsTemplate is the substrate ActorTemplate that runs
	// the Antigravity Interactions harness.
	AntigravityInteractionsTemplate = "ax-harness-interactions-template"
)

// Config represents the main configuration for the AX harness server.
type Config struct {
	Version   string          `yaml:"version"`
	Server    ServerConfig    `yaml:"server"`
	EventLog  EventLogConfig  `yaml:"eventlog"`
	Harnesses HarnessesConfig `yaml:"harnesses,omitempty"`
	// Skills sources skills from the Gemini Enterprise Skill Registry into on-disk folders
	// before the harness starts. It is harness-agnostic: each actor runs exactly
	// one harness, which consumes the materialized folder(s). Optional; disabled
	// when no registry is enabled.
	Skills    SkillsConfig    `yaml:"skills,omitempty"`
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
//   - Built-in harnesses (e.g. Antigravity, AntigravityInteractions) whose
//     implementation and container image are provided by AX.
//   - Custom harnesses on substrate whose implementation and container image are
//     provided by the user via their own ActorTemplate.
type HarnessesConfig struct {
	Antigravity             AntigravityHarnessConfig             `yaml:"antigravity,omitempty"`
	AntigravityInteractions AntigravityInteractionsHarnessConfig `yaml:"antigravity-interactions,omitempty"`
	Substrate               []SubstrateHarnessConfig             `yaml:"substrate,omitempty"`
}

// AntigravityHarnessConfig registers the built-in Antigravity harness.
type AntigravityHarnessConfig struct {
	Default  bool   `yaml:"default,omitempty"`
	Endpoint string `yaml:"endpoint,omitempty"` // HarnessService address
}

// AntigravityInteractionsHarnessConfig registers the built-in Antigravity
// Interactions harness (over the Vertex GenAI Interactions API).
type AntigravityInteractionsHarnessConfig struct {
	Default bool   `yaml:"default,omitempty"` // Default harness or not
	Agent   string `yaml:"agent,omitempty"`   // Interactions API agent (default: antigravityinteractions.DefaultAgent)
	// SystemInstruction is a free-form system prompt sent on every turn.
	SystemInstruction string `yaml:"system_instruction,omitempty"`
}

// SkillsConfig configures optional skill sources (top-level, harness-agnostic).
// Today the only source type is the Gemini Enterprise Skill Registry; it may source from more
// than one registry (e.g. a shared org-wide registry plus a team-specific one),
// each with its own project/location, selection, and target directory.
type SkillsConfig struct {
	Registries []SkillsRegistryConfig `yaml:"registries,omitempty"`
}

// Validate checks the (top-level) skills config.
func (s SkillsConfig) Validate() error {
	for i := range s.Registries {
		if err := s.Registries[i].validate(i); err != nil {
			return err
		}
	}
	return nil
}

// validate enforces that, when this registry source is enabled, exactly one
// selection mode is set (skills, query, or all) — a config-level "oneof". idx is
// the registry's index within the registries list, for error context.
func (rc SkillsRegistryConfig) validate(idx int) error {
	if !rc.Enabled {
		return nil
	}
	if rc.TargetDir == "" {
		return fmt.Errorf("skills.registries[%d] requires target_dir (skills materialize to <target_dir>/<skill-id>/)", idx)
	}
	modes := 0
	if len(rc.Skills) > 0 {
		modes++
	}
	if rc.Query != nil {
		modes++
	}
	if rc.All {
		modes++
	}
	switch {
	case modes == 0:
		return fmt.Errorf("skills.registries[%d] requires exactly one selection mode (set one of skills, query, or all)", idx)
	case modes > 1:
		return fmt.Errorf("skills.registries[%d] sets multiple selection modes; set exactly one of skills, query, or all", idx)
	}
	if rc.Query != nil && rc.Query.Text == "" {
		return fmt.Errorf("skills.registries[%d].query requires a non-empty text", idx)
	}
	return nil
}

// SkillsRegistryConfig sources agentskills.io skills from the Gemini Skill
// Registry. When Enabled, exactly one selection mode should be set (Skills,
// Query, or All); if none is set, all skills are materialized.
type SkillsRegistryConfig struct {
	Enabled bool `yaml:"enabled,omitempty"`
	// Project owns the skills (projects/{Project}/locations/{Location}/skills).
	// Empty falls back to the GOOGLE_CLOUD_PROJECT environment variable.
	Project string `yaml:"project,omitempty"`
	// Location is the registry region, e.g. "us-central1". Empty falls back to
	// GOOGLE_CLOUD_LOCATION, then a built-in default.
	Location string `yaml:"location,omitempty"`

	// --- selection (choose one) ---

	// Skills is an explicit allowlist of skills, each optionally pinned to a
	// revision. Takes precedence over Query and All.
	Skills []SkillRefConfig `yaml:"skills,omitempty"`
	// Query is a semantic search selection; its top matches are materialized.
	// TopK lives inside it because it only has meaning for a query.
	Query *SkillsQueryConfig `yaml:"query,omitempty"`
	// All materializes every skill in the project/location (used when neither
	// Skills nor Query is set; can also be set explicitly).
	All bool `yaml:"all,omitempty"`

	// TargetDir is the base directory skills are materialized into: each skill
	// is written to <TargetDir>/<skill-id>/. Required when Enabled.
	TargetDir string `yaml:"target_dir,omitempty"`
}

// SkillsQueryConfig selects skills by semantic search.
type SkillsQueryConfig struct {
	// Text is the semantic search string (required for query selection).
	Text string `yaml:"text"`
	// TopK bounds the number of matches (<=0 uses the server default).
	TopK int `yaml:"top_k,omitempty"`
}

// SkillRefConfig identifies a skill to materialize, optionally pinned.
type SkillRefConfig struct {
	ID       string `yaml:"id"`
	Revision string `yaml:"revision,omitempty"`
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
	return LoadFromBytes(data)
}

// LoadFromBytes parses configuration from YAML bytes and applies defaults.
func LoadFromBytes(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
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
	if c.Harnesses.AntigravityInteractions.Default {
		defaultCount++
	}

	for _, sc := range c.Harnesses.Substrate {
		if sc.ID == "" {
			return fmt.Errorf("substrate harness id is required")
		}
		if sc.ID == AntigravityHarnessID || sc.ID == AntigravityInteractionsHarnessID {
			return fmt.Errorf("substrate harness id %q is reserved for built-in harnesses", sc.ID)
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

	if err := c.Skills.Validate(); err != nil {
		return err
	}

	return nil
}

func AXAssetsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ".ax"), nil
}
