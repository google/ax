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
)

// WorkspaceSystemInstruction builds a system-instruction snippet that orients
// the agent about its working directory.
//
// This complements the executor making WorkDir authoritative (see
// AntigravityInteractionsConfig.WorkDir): the executor guarantees commands run
// in the right place, while this tells the agent so it emits sensible paths
// (relative to the workspace, not the process root "/"). Returns "" when
// workDir is empty.
func WorkspaceSystemInstruction(workDir string) string {
	if strings.TrimSpace(workDir) == "" {
		return ""
	}
	return fmt.Sprintf("Your working directory is %s. Run commands and resolve "+
		"relative paths there unless you have a specific reason to use an "+
		"absolute path elsewhere.", workDir)
}
