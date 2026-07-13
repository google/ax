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
		Agent: "projects/p/locations/global/agents/a",
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

func TestLoadFromBytes(t *testing.T) {
	cfg, err := LoadFromBytes([]byte(`
version: v1alpha
harnesses:
  antigravity:
    default: true
`))
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	if cfg.Version != "v1alpha" {
		t.Errorf("Version = %q, want %q", cfg.Version, "v1alpha")
	}
	if !cfg.Harnesses.Antigravity.Default {
		t.Error("Harnesses.Antigravity.Default = false, want true")
	}
	// setDefaults must run (same as LoadFromFile).
	if got, want := cfg.Server.Address, ":8494"; got != want {
		t.Errorf("Server.Address = %q, want default %q", got, want)
	}
}

// TestLoadFromBytes_Invalid returns an error on malformed YAML.
func TestLoadFromBytes_Invalid(t *testing.T) {
	if _, err := LoadFromBytes([]byte("harnesses: [unterminated")); err == nil {
		t.Fatal("LoadFromBytes(invalid): got nil error, want error")
	}
}

func TestParse_SkillsByID(t *testing.T) {
	data := `
skills:
  registries:
    - enabled: true
      project: my-proj
      location: us-central1
      target_dir: /tmp/ax-skills
      skills:
        - id: emoji
        - id: lowercase
          revision: rev-3
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(data), &cfg); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if len(cfg.Skills.Registries) != 1 {
		t.Fatalf("registries = %d, want 1", len(cfg.Skills.Registries))
	}
	reg := cfg.Skills.Registries[0]
	if !reg.Enabled || reg.Project != "my-proj" || reg.Location != "us-central1" || reg.TargetDir != "/tmp/ax-skills" {
		t.Errorf("registry = %+v, want enabled my-proj/us-central1 /tmp/ax-skills", reg)
	}
	if len(reg.Skills) != 2 {
		t.Fatalf("skills = %d, want 2", len(reg.Skills))
	}
	if reg.Skills[0].ID != "emoji" || reg.Skills[0].Revision != "" {
		t.Errorf("skills[0] = %+v, want {emoji }", reg.Skills[0])
	}
	if reg.Skills[1].ID != "lowercase" || reg.Skills[1].Revision != "rev-3" {
		t.Errorf("skills[1] = %+v, want {lowercase rev-3}", reg.Skills[1])
	}
	if reg.Query != nil {
		t.Errorf("Query = %+v, want nil in by-id mode", reg.Query)
	}
}

func TestParse_SkillsByQuery(t *testing.T) {
	data := `
skills:
  registries:
    - enabled: true
      project: my-proj
      target_dir: /tmp/ax-skills
      query:
        text: "find gcp skills"
        top_k: 5
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(data), &cfg); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	regs := cfg.Skills.Registries
	if len(regs) != 1 || regs[0].Query == nil {
		t.Fatalf("registries = %+v, want one with a query block", regs)
	}
	if regs[0].Query.Text != "find gcp skills" || regs[0].Query.TopK != 5 {
		t.Errorf("Query = %+v, want {find gcp skills 5}", *regs[0].Query)
	}
}

func TestParse_MultipleRegistries(t *testing.T) {
	data := `
skills:
  registries:
    - enabled: true
      project: org-proj
      target_dir: /tmp/org
      all: true
    - enabled: true
      project: team-proj
      target_dir: /tmp/team
      skills:
        - id: teamskill
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(data), &cfg); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	regs := cfg.Skills.Registries
	if len(regs) != 2 {
		t.Fatalf("registries = %d, want 2", len(regs))
	}
	if regs[0].Project != "org-proj" || regs[0].TargetDir != "/tmp/org" || !regs[0].All {
		t.Errorf("registries[0] = %+v, want org-proj /tmp/org all", regs[0])
	}
	if regs[1].Project != "team-proj" || regs[1].TargetDir != "/tmp/team" || len(regs[1].Skills) != 1 {
		t.Errorf("registries[1] = %+v, want team-proj /tmp/team [teamskill]", regs[1])
	}
	// Each registry has exactly one selection mode + target_dir, so validation passes.
	if err := cfg.Skills.Validate(); err != nil {
		t.Errorf("Skills.Validate() = %v, want nil", err)
	}
}

func TestValidate_SkillsSelectionOneof(t *testing.T) {
	// withRegistry returns a valid config carrying a single top-level registry.
	// It fills TargetDir (a required field) unless the caller already set one, so
	// selection-mode assertions aren't masked by the target_dir check.
	withRegistry := func(rc SkillsRegistryConfig) *Config {
		if rc.Enabled && rc.TargetDir == "" {
			rc.TargetDir = "/tmp/skills"
		}
		c := validConfig()
		c.Skills.Registries = []SkillsRegistryConfig{rc}
		return c
	}

	t.Run("disabled skips validation", func(t *testing.T) {
		if err := withRegistry(SkillsRegistryConfig{Enabled: false}).Validate(); err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
	})

	t.Run("enabled without target_dir is an error", func(t *testing.T) {
		c := validConfig()
		c.Skills.Registries = []SkillsRegistryConfig{
			{Enabled: true, Project: "p", All: true}, // valid mode, but no target_dir
		}
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "target_dir") {
			t.Fatalf("Validate() = %v, want target_dir error", err)
		}
	})

	t.Run("zero selection modes is an error", func(t *testing.T) {
		err := withRegistry(SkillsRegistryConfig{Enabled: true, Project: "p"}).Validate()
		if err == nil || !strings.Contains(err.Error(), "exactly one selection mode") {
			t.Fatalf("Validate() = %v, want exactly-one error", err)
		}
	})

	t.Run("multiple selection modes is an error", func(t *testing.T) {
		err := withRegistry(SkillsRegistryConfig{
			Enabled: true, Project: "p",
			Skills: []SkillRefConfig{{ID: "emoji"}},
			All:    true,
		}).Validate()
		if err == nil || !strings.Contains(err.Error(), "multiple selection modes") {
			t.Fatalf("Validate() = %v, want multiple-modes error", err)
		}
	})

	t.Run("exactly one is valid", func(t *testing.T) {
		err := withRegistry(SkillsRegistryConfig{
			Enabled: true, Project: "p",
			Skills: []SkillRefConfig{{ID: "emoji"}},
		}).Validate()
		if err != nil {
			t.Fatalf("Validate() = %v, want nil", err)
		}
	})

	t.Run("query with empty text is an error", func(t *testing.T) {
		err := withRegistry(SkillsRegistryConfig{
			Enabled: true, Project: "p",
			Query: &SkillsQueryConfig{Text: ""},
		}).Validate()
		if err == nil || !strings.Contains(err.Error(), "non-empty text") {
			t.Fatalf("Validate() = %v, want empty-text error", err)
		}
	})

	t.Run("second registry invalid is caught", func(t *testing.T) {
		c := validConfig()
		c.Skills.Registries = []SkillsRegistryConfig{
			{Enabled: true, Project: "p", TargetDir: "/tmp/a", All: true},
			{Enabled: true, Project: "p", TargetDir: "/tmp/b"}, // no selection mode
		}
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "registries[1]") {
			t.Fatalf("Validate() = %v, want error citing registries[1]", err)
		}
	})
}
