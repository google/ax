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

package workspace

import (
	"strings"
	"testing"
	"time"
)

func TestRunSetupCommandWithTimeout(t *testing.T) {
	out, err := runSetupCommandWithTimeout(t.Context(), "echo starting && sleep 30", t.TempDir(), 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("runSetupCommandWithTimeout() = %v, want error containing %q", err, "timed out")
	}
	if !strings.Contains(string(out), "starting") {
		t.Errorf("output = %q, want partial stdout before timeout", out)
	}
}
