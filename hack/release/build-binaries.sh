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

# Builds the ax CLI release archives.
#
#   hack/release/build-binaries.sh <version> [out-dir]
#
# When AX_SERVER_IMAGE is set to a digest-pinned reference (<repo>@sha256:...),
# its digest is stamped into internal/version.ServerImageDigest so release
# builds default `--server-image` to `<version>@sha256:<digest>` without a
# runtime tag lookup.
#
# Writes to out-dir (default bin/release), for every platform in CLI_PLATFORMS
# (hack/release/lib.sh):
#
#   ax_<os>_<arch>.tar.gz  holding the static `ax` binary, which reports
#                          <version> from `ax version`
#   checksums.txt          "<sha256>  <archive>" for every archive
#
# plus install.sh from the repository root, if there is one, so that
# https://github.com/google/ax/releases/latest/download/install.sh serves the
# installer that matches the archives.
#
# Needs Go, GNU tar, gzip, and sha256sum (coreutils); on macOS, use gtar and
# coreutils from Homebrew.

set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=hack/release/lib.sh
source "${ROOT}/hack/release/lib.sh"

VERSION="${1:-}"
OUT="${2:-${ROOT}/bin/release}"
require_release_version "${VERSION}"

mkdir -p "${OUT}"
OUT="$(cd "${OUT}" && pwd)"
cd "${ROOT}"

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

# If AX_SERVER_IMAGE is set to a digest-pinned reference (<repo>@sha256:...),
# the ax binary is also stamped with version.ServerImageDigest so release
# builds pin their matching ax-server image without relying on a mutable tag.
server_digest=""
if [[ -n "${AX_SERVER_IMAGE:-}" ]]; then
  if [[ "${AX_SERVER_IMAGE}" != *@* ]]; then
    echo "error: AX_SERVER_IMAGE must carry a @sha256:<64hex> digest (got '${AX_SERVER_IMAGE}')" >&2
    exit 1
  fi
  server_digest="${AX_SERVER_IMAGE##*@}"
  if [[ ! "${server_digest}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "error: AX_SERVER_IMAGE must carry a @sha256:<64hex> digest (got '${AX_SERVER_IMAGE}')" >&2
    exit 1
  fi
fi

archives=()
for platform in "${CLI_PLATFORMS[@]}"; do
  os="${platform%/*}"
  arch="${platform#*/}"
  archive="ax_${os}_${arch}.tar.gz"
  echo "==> Building ${archive} (${VERSION})"
  mkdir -p "${work}/${os}_${arch}"
  GOOS="${os}" GOARCH="${arch}" CGO_ENABLED=0 \
    go build -trimpath -ldflags="$(go_ldflags "${VERSION}" "${server_digest}")" \
    -o "${work}/${os}_${arch}/ax" ./cmd/ax
  # Fixed owner, mode, order, and timestamps (tar and gzip) so the archive
  # depends only on the binary.
  tar --create --directory "${work}/${os}_${arch}" \
    --owner=0 --group=0 --numeric-owner --mode=0755 --mtime=@0 --sort=name ax \
    | gzip -n -9 > "${OUT}/${archive}"
  archives+=("${archive}")
done

(cd "${OUT}" && sha256sum "${archives[@]}" > checksums.txt)
echo "==> Wrote ${OUT}/checksums.txt"

if [[ -f "${ROOT}/install.sh" ]]; then
  install -m 0755 "${ROOT}/install.sh" "${OUT}/install.sh"
  echo "==> Copied install.sh"
fi
