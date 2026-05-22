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

package controller2

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var reservedAgentIDs = map[string]struct{}{
	"gemini":    {},
	"__planner": {},
}

var validIDRegex = regexp.MustCompile(`^[A-Za-z0-9\-_]+$`)

// validateID checks if an ID contains allowed characters and is not reserved.
func validateID(id string) error {
	if id == "" {
		return errors.New("empty ID")
	}

	if !validIDRegex.MatchString(id) {
		return fmt.Errorf("invalid ID %q: must only contain A-Z, a-z, 0-9, -, and _", id)
	}

	if _, isReserved := reservedAgentIDs[strings.ToLower(id)]; isReserved {
		return fmt.Errorf("agent ID %q is reserved", id)
	}

	return nil
}
