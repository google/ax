#!/usr/bin/env bash

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

# Builds the ax CLI release binaries.
#
#   hack/release/build-binaries.sh <version> [out-dir]
#
# Writes ax-<os>-<arch> and ax-<os>-<arch>.sha256 for every platform in
# CLI_PLATFORMS (hack/release/lib.sh) to out-dir (default bin/release). The
# binaries are static and report <version> from `ax version`.

set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=hack/release/lib.sh
source "${ROOT}/hack/release/lib.sh"

VERSION="${1:-}"
OUT="${2:-${ROOT}/bin/release}"
require_release_version "${VERSION}"

mkdir -p "${OUT}"
cd "${ROOT}"

for platform in "${CLI_PLATFORMS[@]}"; do
  os="${platform%/*}"
  arch="${platform#*/}"
  name="ax-${os}-${arch}"
  echo "==> Building ${name} (${VERSION})"
  GOOS="${os}" GOARCH="${arch}" CGO_ENABLED=0 \
    go build -trimpath -ldflags="$(go_ldflags "${VERSION}")" -o "${OUT}/${name}" ./cmd/ax
  # Written relative to OUT so `sha256sum -c` works after download.
  (cd "${OUT}" && sha256sum "${name}" > "${name}.sha256")
done
