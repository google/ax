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

package version

import "testing"

func TestImage(t *testing.T) {
	orig := Version
	t.Cleanup(func() { Version = orig })

	for _, tc := range []struct {
		version string
		want    string
	}{
		{"v0.4.0", "ghcr.io/google/ax/ax-server:v0.4.0"},
		{"v0.4.0-rc.1", "ghcr.io/google/ax/ax-server:v0.4.0-rc.1"},
		{"dev", "ghcr.io/google/ax/ax-server:latest"},
		{"", "ghcr.io/google/ax/ax-server:latest"},
		{"v0.4", "ghcr.io/google/ax/ax-server:latest"},
		{"v0.4.0+dirty", "ghcr.io/google/ax/ax-server:latest"},
	} {
		Version = tc.version
		if got := Image("ax-server"); got != tc.want {
			t.Errorf("Version=%q: Image(ax-server) = %q, want %q", tc.version, got, tc.want)
		}
	}
}
