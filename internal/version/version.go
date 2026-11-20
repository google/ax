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

// Package version reports the AX release a binary was built from, and the
// published release images that match it.
package version

import (
	"regexp"
	"runtime/debug"
)

// ImageRepo is where releases publish the AX images
// (.github/workflows/release.yml), such as ImageRepo + "/ax-server" and
// ImageRepo + "/ax-runner". RELEASE_IMAGES in hack/release/lib.sh is the
// full list.
const ImageRepo = "ghcr.io/google/ax"

// Version is the AX release this binary was built from. Release builds set it
// with -ldflags "-X github.com/google/ax/internal/version.Version=vX.Y.Z"
// (hack/release/lib.sh); any other build reports "dev".
var Version = "dev"

var releaseVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// IsRelease reports whether Version is a release version
// (vX.Y.Z or vX.Y.Z-PRERELEASE).
func IsRelease() bool {
	return releaseVersion.MatchString(Version)
}

// ImageTag is the image tag matching this binary: Version for a release build,
// and "latest" (the newest stable release) for any other build. :latest exists
// only once a stable release has been published.
func ImageTag() string {
	if IsRelease() {
		return Version
	}
	return "latest"
}

// Image returns the published reference of release image name (for example
// "ax-server") that matches this binary.
func Image(name string) string {
	return ImageRepo + "/" + name + ":" + ImageTag()
}

// String is the version `ax version` prints. A release build prints Version.
// Any other build prints "dev" and, when the Go toolchain recorded it, the
// commit it was built from ("dev+8e2a187a622c", with "-dirty" for a modified
// checkout), so a report from a source build names its commit.
func String() string {
	if Version != "dev" {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return Version
	}
	return devVersion(info.Settings)
}

func devVersion(settings []debug.BuildSetting) string {
	var revision string
	var modified bool
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision == "" {
		return "dev"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if modified {
		revision += "-dirty"
	}
	return "dev+" + revision
}
