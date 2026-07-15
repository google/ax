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

// Package skills defines the mode-agnostic result shape that describes which
// agent skills are available on disk and where. Both skill sources -- the
// Gemini Enterprise Skill Registry (internal/skills/geminienterprise) and the
// local directory mode (internal/skills/local) -- produce this same shape so a
// single consumer (e.g. a harness system-instruction builder) can serve both
// without depending on either source package.
package skills

// Available reports the skills discovered/materialized on disk, grouped by the
// directory they live in. It is the common currency between skill sources and
// consumers.
type Available struct {
	// Groups is one entry per source that produced skills (a registry's
	// target_dir, or a local skills directory).
	Groups []Group
}

// Group associates a set of skills with the directory that contains them. Each
// skill lives at <Dir>/<skill-id>/ (with a SKILL.md inside).
type Group struct {
	Dir    string
	Skills []Skill
}

// Skill identifies one on-disk skill.
type Skill struct {
	// ID is the skill identifier, which is also its directory name under the
	// group's Dir.
	ID string
	// Dir is the absolute path of the skill's own folder (<group Dir>/<ID>/).
	Dir string
}

// Empty reports whether nothing is available.
func (a Available) Empty() bool { return len(a.Groups) == 0 }
