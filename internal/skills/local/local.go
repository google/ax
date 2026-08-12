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

// Package local discovers agent skills that already exist on disk. It is the
// local-directory counterpart to internal/skills/geminienterprise: instead of
// fetching from the Gemini Enterprise Skill Registry, it treats a configured
// directory as a ready-made skills folder and reports what it finds.
//
// A local skills path is a parent directory containing one subfolder per skill,
// each with a SKILL.md (the same layout as examples/skills and a registry's
// target_dir). Nothing is written; the directory is used as-is.
//
// It is harness-agnostic and produces []skills.Group, so the same consumers that
// handle registry skills handle local ones.
package local

import (
	"log"
	"os"
	"path/filepath"

	"github.com/google/ax/internal/config"
	"github.com/google/ax/internal/skills"
)

// skillManifest is the file that marks a subdirectory as a skill.
const skillManifest = "SKILL.md"

// Discover enumerates every enabled local skills path in sc and reports the
// skills found on disk. Each enabled path is a parent directory whose immediate
// subfolders containing a SKILL.md are treated as skills (subfolder name = skill
// id).
//
// It is fail-safe, mirroring geminienterprise.Materialize: disabled entries are
// skipped, and any path that is missing, unreadable, not a directory, or
// contains no skills is logged and skipped so a local-skills problem never
// blocks harness creation. Paths are resolved to absolute form so the reported
// directories are stable regardless of the process working directory.
func Discover(sc config.SkillsConfig) []skills.Group {
	var groups []skills.Group
	for i := range sc.Local {
		lc := sc.Local[i]
		if !lc.Enabled {
			continue
		}
		group, ok := discoverOne(lc.Path, i)
		if !ok {
			continue
		}
		groups = append(groups, group)
	}
	return groups
}

// discoverOne scans a single local skills path. idx is the entry's index within
// the local list, for log context. It returns ok=false (and logs the reason)
// when the path yields no usable skills.
func discoverOne(path string, idx int) (skills.Group, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		log.Printf("skills: local[%d] %q: resolving absolute path: %v; skipping", idx, path, err)
		return skills.Group{}, false
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		log.Printf("skills: local[%d] %q: reading directory: %v; skipping", idx, abs, err)
		return skills.Group{}, false
	}

	group := skills.Group{Dir: abs}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		skillDir := filepath.Join(abs, e.Name())
		manifest := filepath.Join(skillDir, skillManifest)
		if info, err := os.Stat(manifest); err != nil || info.IsDir() {
			// No SKILL.md (or it is a directory): not a skill. Silently skip so
			// non-skill entries (e.g. a README) don't produce log noise.
			continue
		}
		group.Skills = append(group.Skills, skills.Skill{ID: e.Name(), Dir: skillDir})
	}

	if len(group.Skills) == 0 {
		log.Printf("skills: local[%d] %q: no skills found (no <skill-id>/%s subfolders); skipping", idx, abs, skillManifest)
		return skills.Group{}, false
	}
	log.Printf("skills: local[%d] discovered %d skill(s) in %s", idx, len(group.Skills), abs)
	return group, true
}
