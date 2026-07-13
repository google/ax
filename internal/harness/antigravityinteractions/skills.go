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

package antigravityinteractions

import (
	"fmt"
	"strings"

	"github.com/google/ax/internal/skills/geminienterprise"
)

// SkillsSystemInstruction builds a system-instruction pointer telling the agent
// where its materialized skills live and lists them.
//
// This is discovery logic specific to the Antigravity Interactions harness: it
// has no SKILLS_DIR concept, so it must be told where to find skills via its
// system instruction (its built-in file tools then read that directory).
// Harnesses that auto-discover a skills directory (e.g. the Antigravity SDK
// harness via SKILLS_DIR) do not use this.
func SkillsSystemInstruction(res geminienterprise.Result) string {
	if res.Empty() {
		return ""
	}
	var b strings.Builder
	for _, w := range res.Written {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "Agent skills are available under %s. Available skills:", w.Dir)
		for _, s := range w.Skills {
			fmt.Fprintf(&b, " %s", s.SkillID)
		}
		b.WriteString(". Read a skill's SKILL.md before using it.")
	}
	return b.String()
}

// JoinSystemInstruction combines a user-configured system instruction with an
// optional skills pointer (from SkillsSystemInstruction), dropping empties.
func JoinSystemInstruction(base, pointer string) string {
	parts := make([]string, 0, 2)
	if strings.TrimSpace(base) != "" {
		parts = append(parts, base)
	}
	if strings.TrimSpace(pointer) != "" {
		parts = append(parts, pointer)
	}
	return strings.Join(parts, "\n\n")
}
