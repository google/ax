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

package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSubstrateNewHarness(t *testing.T) {
	h, err := SubstrateHarnessConfig{ID: "c", Namespace: "team-ns", Template: "custom-template"}.NewHarness("api.ate-system.svc:443")
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	if h == nil {
		t.Fatal("expected non-nil harness")
	}
}

// validConfig returns a config that passes Validate, that tests can mutate.
func validConfig() *Config {
	c := DefaultConfig()
	c.Harnesses = HarnessesConfig{
		Antigravity: AntigravityHarnessConfig{Default: true},
		Substrate: []SubstrateHarnessConfig{
			{ID: "custom", Namespace: "team-ns", Template: "custom-template"},
		},
	}
	return c
}

func TestValidate_ValidConfig(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidate_CustomIDRequired(t *testing.T) {
	c := validConfig()
	c.Harnesses.Substrate[0].ID = ""
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "substrate harness id") {
		t.Fatalf("Validate() = %v, want substrate id error", err)
	}
}

func TestValidate_CustomIDReserved(t *testing.T) {
	c := validConfig()
	c.Harnesses.Substrate[0].ID = "antigravity"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("Validate() = %v, want reserved id error", err)
	}
}

func TestValidate_CustomNamespaceRequired(t *testing.T) {
	c := validConfig()
	c.Harnesses.Substrate[0].Namespace = ""
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "namespace is required") {
		t.Fatalf("Validate() = %v, want namespace-required error", err)
	}
}

func TestValidate_CustomNamespaceReserved(t *testing.T) {
	c := validConfig()
	c.Harnesses.Substrate[0].Namespace = defaultNamespace
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("Validate() = %v, want reserved-namespace error", err)
	}
}

func TestValidate_CustomTemplateRequired(t *testing.T) {
	c := validConfig()
	c.Harnesses.Substrate[0].Template = ""
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "template is required") {
		t.Fatalf("Validate() = %v, want template-required error", err)
	}
}

func TestValidate_MultipleDefaults(t *testing.T) {
	c := validConfig()
	c.Harnesses.Substrate[0].Default = true
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "multiple harnesses marked as default") {
		t.Fatalf("Validate() = %v, want multiple defaults error", err)
	}
}

func TestValidate_InteractionsIDReserved(t *testing.T) {
	c := validConfig()
	c.Harnesses.Substrate[0].ID = "antigravity-interactions"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("Validate() = %v, want reserved id error", err)
	}
}

func TestValidate_InteractionsValid(t *testing.T) {
	c := validConfig()
	c.Harnesses.AntigravityInteractions = AntigravityInteractionsHarnessConfig{
		Agent:    "projects/p/locations/global/agents/a",
		StateDir: "interactions-state",
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestLoadFromFile_Version(t *testing.T) {
	data := `
version: "1.2.3"
server:
  address: ":8080"
eventlog:
  sqlite:
    filename: "test.db"
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(data), &cfg); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if cfg.Version != "1.2.3" {
		t.Errorf("cfg.Version = %q, want %q", cfg.Version, "1.2.3")
	}
}
