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

package geminienterprise

import (
	"context"
	"testing"

	"github.com/google/ax/internal/config"
)

func TestSelectionFromConfig(t *testing.T) {
	t.Run("explicit skills take precedence", func(t *testing.T) {
		rc := config.SkillsRegistryConfig{
			Skills: []config.SkillRefConfig{{ID: "emoji"}, {ID: "lowercase", Revision: "rev-3"}},
			Query:  &config.SkillsQueryConfig{Text: "ignored"},
		}
		sel := selectionFromConfig(rc)
		if sel.All || sel.Query != "" {
			t.Fatalf("got %+v, want by-id", sel)
		}
		if len(sel.SkillRefs) != 2 ||
			sel.SkillRefs[0].SkillID != "emoji" || sel.SkillRefs[0].Revision != "" ||
			sel.SkillRefs[1].SkillID != "lowercase" || sel.SkillRefs[1].Revision != "rev-3" {
			t.Fatalf("refs = %+v", sel.SkillRefs)
		}
	})

	t.Run("query when no explicit skills", func(t *testing.T) {
		sel := selectionFromConfig(config.SkillsRegistryConfig{
			Query: &config.SkillsQueryConfig{Text: "find gcp", TopK: 5},
		})
		if sel.Query != "find gcp" || sel.TopK != 5 {
			t.Fatalf("got %+v, want query=find gcp topK=5", sel)
		}
	})

	t.Run("empty query text falls through to all", func(t *testing.T) {
		sel := selectionFromConfig(config.SkillsRegistryConfig{Query: &config.SkillsQueryConfig{Text: "  "}})
		if !sel.All {
			t.Fatalf("got %+v, want All (blank query text)", sel)
		}
	})

	t.Run("all when nothing set", func(t *testing.T) {
		if sel := selectionFromConfig(config.SkillsRegistryConfig{}); !sel.All {
			t.Fatalf("got %+v, want All", sel)
		}
	})
}

func TestClaimSet_FirstWins(t *testing.T) {
	cs := newClaimSet()
	if won, by := cs.claim("/dir", "s1", 0); !won || by != 0 {
		t.Fatalf("first claim = (%v,%d), want (true,0)", won, by)
	}
	// Same (dir,id) again -> loses, reports the first registry (0).
	if won, by := cs.claim("/dir", "s1", 2); won || by != 0 {
		t.Fatalf("dup claim = (%v,%d), want (false,0)", won, by)
	}
	// Same id, different dir -> independent, wins.
	if won, _ := cs.claim("/other", "s1", 1); !won {
		t.Fatal("same id in different dir should win")
	}
	// Different id, same dir -> wins.
	if won, _ := cs.claim("/dir", "s2", 1); !won {
		t.Fatal("different id in same dir should win")
	}
}

func TestMaterialize_DisabledIsEmpty(t *testing.T) {
	// Disabled config => no-op, empty result, no error/panic.
	if res := Materialize(context.Background(), config.SkillsConfig{}); !res.Empty() {
		t.Errorf("disabled Materialize = %+v, want empty", res)
	}
}

func TestMaterialize_EnabledNoProjectIsEmpty(t *testing.T) {
	// Enabled with a target_dir but no project (config empty, GOOGLE_CLOUD_PROJECT
	// unset) => fail-safe empty result (no panic, no materialization).
	t.Setenv(envCloudProject, "")
	sc := config.SkillsConfig{Registries: []config.SkillsRegistryConfig{
		{Enabled: true, TargetDir: t.TempDir()},
	}}
	if res := Materialize(context.Background(), sc); !res.Empty() {
		t.Errorf("no-project Materialize = %+v, want empty", res)
	}
}
