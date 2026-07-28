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

package local

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/google/ax/internal/config"
)

// writeSkillDir creates <parent>/<id>/SKILL.md so the id is discovered as a
// skill.
func writeSkillDir(t *testing.T, parent, id string) {
	t.Helper()
	dir := filepath.Join(parent, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, skillManifest), []byte("# "+id+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscover_FindsSkillSubdirs(t *testing.T) {
	root := t.TempDir()
	writeSkillDir(t, root, "emoji")
	writeSkillDir(t, root, "lowercase")
	// A non-skill subdir (no SKILL.md) and a stray file must be ignored.
	if err := os.MkdirAll(filepath.Join(root, "not-a-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	avail := Discover(config.SkillsConfig{Local: []config.LocalSkillsConfig{
		{Enabled: true, Path: root},
	}})
	if avail.Empty() {
		t.Fatal("expected skills, got none")
	}
	if len(avail.Groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(avail.Groups))
	}
	g := avail.Groups[0]
	// Dir is reported as an absolute path.
	if !filepath.IsAbs(g.Dir) {
		t.Errorf("group Dir = %q, want absolute", g.Dir)
	}
	var got []string
	for _, s := range g.Skills {
		got = append(got, s.ID)
		wantDir := filepath.Join(g.Dir, s.ID)
		if s.Dir != wantDir {
			t.Errorf("skill %q Dir = %q, want %q", s.ID, s.Dir, wantDir)
		}
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "emoji" || got[1] != "lowercase" {
		t.Errorf("skill ids = %v, want [emoji lowercase]", got)
	}
}

func TestDiscover_DisabledIsSkipped(t *testing.T) {
	root := t.TempDir()
	writeSkillDir(t, root, "emoji")
	avail := Discover(config.SkillsConfig{Local: []config.LocalSkillsConfig{
		{Enabled: false, Path: root},
	}})
	if !avail.Empty() {
		t.Errorf("disabled entry produced skills: %+v", avail)
	}
}

func TestDiscover_MissingPathDegrades(t *testing.T) {
	// A non-existent path must not panic or error; it degrades to no skills.
	avail := Discover(config.SkillsConfig{Local: []config.LocalSkillsConfig{
		{Enabled: true, Path: filepath.Join(t.TempDir(), "does-not-exist")},
	}})
	if !avail.Empty() {
		t.Errorf("missing path produced skills: %+v", avail)
	}
}

func TestDiscover_EmptyDirDegrades(t *testing.T) {
	// A directory with no <skill-id>/SKILL.md subfolders yields no skills.
	avail := Discover(config.SkillsConfig{Local: []config.LocalSkillsConfig{
		{Enabled: true, Path: t.TempDir()},
	}})
	if !avail.Empty() {
		t.Errorf("empty dir produced skills: %+v", avail)
	}
}

func TestDiscover_MultiplePaths(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	writeSkillDir(t, a, "s1")
	writeSkillDir(t, b, "s2")
	avail := Discover(config.SkillsConfig{Local: []config.LocalSkillsConfig{
		{Enabled: true, Path: a},
		{Enabled: true, Path: b},
	}})
	if len(avail.Groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(avail.Groups))
	}
	var got []string
	for _, g := range avail.Groups {
		for _, s := range g.Skills {
			got = append(got, s.ID)
		}
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "s1" || got[1] != "s2" {
		t.Errorf("skill ids = %v, want [s1 s2]", got)
	}
}

// TestDiscover_ExamplesSkillsFixture points at the repo's real examples/skills
// directory to guard the expected on-disk layout (parent dir of <skill>/SKILL.md
// subfolders, with a README.md that must be ignored).
func TestDiscover_ExamplesSkillsFixture(t *testing.T) {
	// This test file lives at internal/skills/local/; examples/skills is at the
	// repo root.
	examples := filepath.Join("..", "..", "..", "examples", "skills")
	if _, err := os.Stat(examples); err != nil {
		t.Skipf("examples/skills not found (%v); skipping", err)
	}
	avail := Discover(config.SkillsConfig{Local: []config.LocalSkillsConfig{
		{Enabled: true, Path: examples},
	}})
	if avail.Empty() {
		t.Fatal("expected skills from examples/skills, got none")
	}
	var got []string
	for _, g := range avail.Groups {
		for _, s := range g.Skills {
			got = append(got, s.ID)
		}
	}
	sort.Strings(got)
	// emoji and lowercase ship in examples/skills; README.md must be ignored.
	if len(got) != 2 || got[0] != "emoji" || got[1] != "lowercase" {
		t.Errorf("examples/skills ids = %v, want [emoji lowercase]", got)
	}
}
