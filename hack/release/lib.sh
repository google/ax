# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Shared helpers for the hack/release scripts. Source, do not execute.
# shellcheck shell=bash disable=SC2034  # the arrays are read by the sourcing scripts

# Release images, in the order they are built. Each is built from
# Dockerfile.<name minus the ax- prefix> and published as
# $AX_IMAGE_REPO/<name>:<version>. Adding an image here is what publishes it.
RELEASE_IMAGES=(ax-server ax-task-runner)

# Platforms the release images and the linux CLI are built for.
RELEASE_PLATFORMS=(linux/amd64 linux/arm64)

# Platforms the ax CLI is built for.
CLI_PLATFORMS=(linux/amd64 linux/arm64 darwin/amd64 darwin/arm64)

# require_release_version fails unless $1 is vMAJOR.MINOR.PATCH or
# vMAJOR.MINOR.PATCH-PRERELEASE. The workflow's tag filter cannot express this,
# so every script checks it before building or publishing anything.
require_release_version() {
  local v="${1:-}"
  if [[ ! "${v}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
    echo "error: '${v}' is not a release version (vX.Y.Z or vX.Y.Z-PRERELEASE)" >&2
    exit 1
  fi
}

# is_prerelease succeeds if $1 has a -PRERELEASE suffix.
is_prerelease() {
  [[ "$1" == *-* ]]
}

# is_latest_stable succeeds if $1 is a stable tag and the highest stable tag in
# the clone. Such a release is marked Latest and its images are also tagged
# :latest, which is what dev builds of `ax install` deploy. Needs every tag
# fetched.
is_latest_stable() {
  is_prerelease "$1" && return 1
  local highest
  highest="$(git tag --list 'v*' --sort=-v:refname \
    | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -n1 || true)"
  [[ -n "${highest}" && "${highest}" == "$1" ]]
}

# go_ldflags prints the -ldflags for a release build of version $1.
go_ldflags() {
  echo "-s -w -X github.com/google/ax/internal/version.Version=$1"
}

# release_state prints the state of the GitHub release for tag $1 in
# repository $2: none, draft, or published. Any error other than "release not
# found" fails, so a flaky API never reads as "no release".
release_state() {
  local out err rc=0
  err="$(mktemp)"
  out="$(gh release view "$1" --repo "$2" --json isDraft --jq '.isDraft' 2>"${err}")" || rc=$?
  if [[ "${rc}" -ne 0 ]]; then
    if grep -q 'release not found' "${err}"; then
      rm -f "${err}"
      echo none
      return 0
    fi
    echo "error: cannot read release $1 in $2: $(cat "${err}")" >&2
    rm -f "${err}"
    return 1
  fi
  rm -f "${err}"
  case "${out}" in
    true) echo draft ;;
    false) echo published ;;
    *) echo "error: unexpected isDraft '${out}' for release $1" >&2; return 1 ;;
  esac
}

# require_on_main fails unless tag $1 points at a commit on origin/main or an
# origin/release-* branch, so a tag on an unreviewed branch cannot publish.
# Needs those branches fetched.
require_on_main() {
  local b
  for b in origin/main $(git for-each-ref --format='%(refname:short)' 'refs/remotes/origin/release-*'); do
    if git merge-base --is-ancestor "$1^{commit}" "${b}" 2>/dev/null; then
      return 0
    fi
  done
  echo "error: $1 is not on origin/main or an origin/release-* branch" >&2
  return 1
}
