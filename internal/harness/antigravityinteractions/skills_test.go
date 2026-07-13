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
	"strings"
	"testing"

	"github.com/google/ax/internal/skills/geminienterprise"
)

func TestSkillsSystemInstruction(t *testing.T) {
	t.Run("empty result yields empty pointer", func(t *testing.T) {
		if got := SkillsSystemInstruction(geminienterprise.Result{}); got != "" {
			t.Errorf("empty result pointer = %q, want empty", got)
		}
	})

	t.Run("mentions dir and skill ids", func(t *testing.T) {
		res := geminienterprise.Result{Written: []geminienterprise.Written{{
			Dir:    "/workspace",
			Skills: []geminienterprise.MaterializedSkill{{SkillID: "emoji"}, {SkillID: "lowercase"}},
		}}}
		got := SkillsSystemInstruction(res)
		if !strings.Contains(got, "/workspace") || !strings.Contains(got, "emoji") || !strings.Contains(got, "lowercase") {
			t.Errorf("pointer = %q, want it to mention dir and skill ids", got)
		}
	})

	t.Run("multiple registries produce multiple lines", func(t *testing.T) {
		res := geminienterprise.Result{Written: []geminienterprise.Written{
			{Dir: "/a", Skills: []geminienterprise.MaterializedSkill{{SkillID: "s1"}}},
			{Dir: "/b", Skills: []geminienterprise.MaterializedSkill{{SkillID: "s2"}}},
		}}
		got := SkillsSystemInstruction(res)
		if !strings.Contains(got, "/a") || !strings.Contains(got, "/b") || !strings.Contains(got, "\n") {
			t.Errorf("pointer = %q, want both dirs on separate lines", got)
		}
	})
}

func TestJoinSystemInstruction(t *testing.T) {
	cases := []struct {
		base, ptr, want string
	}{
		{"", "", ""},
		{"base only", "", "base only"},
		{"", "ptr only", "ptr only"},
		{"base", "ptr", "base\n\nptr"},
	}
	for _, c := range cases {
		if got := JoinSystemInstruction(c.base, c.ptr); got != c.want {
			t.Errorf("join(%q,%q) = %q, want %q", c.base, c.ptr, got, c.want)
		}
	}
}
