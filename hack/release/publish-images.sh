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

# Builds and pushes the multi-platform AX release images.
#
#   AX_IMAGE_REPO=ghcr.io/google/ax hack/release/publish-images.sh <version> [out-dir]
#
# Pushes $AX_IMAGE_REPO/<image>:<version> for every image in RELEASE_IMAGES,
# for every platform in RELEASE_PLATFORMS (hack/release/lib.sh), and writes
# out-dir/images.env (default bin/release) with the digest-pinned references
# deploy/ax-server.yaml takes:
#
#   AX_SERVER_IMAGE=<repo>/ax-server@sha256:...
#   AX_DEFAULT_TASK_IMAGE=<repo>/ax-task-runner@sha256:...
#
# If every image already exists for <version> on every platform, nothing is
# built and the existing digests are recorded, so a rerun never changes the
# digests of a release. Set AX_PUBLISH_OVERWRITE=true to rebuild and replace
# them anyway. An incomplete set is always rebuilt in full.
#
# Needs Go, docker buildx with a builder that can push multi-platform images
# (docker buildx create --driver docker-container --use) and QEMU for the
# non-native platform, jq, and a docker login to the registry.

set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=hack/release/lib.sh
source "${ROOT}/hack/release/lib.sh"

VERSION="${1:-}"
OUT="${2:-${ROOT}/bin/release}"
OVERWRITE="${AX_PUBLISH_OVERWRITE:-false}"
require_release_version "${VERSION}"
if [[ -z "${AX_IMAGE_REPO:-}" ]]; then
  echo "error: AX_IMAGE_REPO is not set (e.g. AX_IMAGE_REPO=ghcr.io/google/ax)" >&2
  exit 1
fi

mkdir -p "${OUT}"
cd "${ROOT}"

PLATFORMS_CSV="$(IFS=,; echo "${RELEASE_PLATFORMS[*]}")"

# existing_digest prints the index digest of $1 if it exists and holds an
# image for every platform in RELEASE_PLATFORMS; otherwise it prints nothing.
existing_digest() {
  local raw
  raw="$(docker buildx imagetools inspect --raw "$1" 2>/dev/null)" || return 0
  local p os arch
  for p in "${RELEASE_PLATFORMS[@]}"; do
    os="${p%/*}"
    arch="${p#*/}"
    if ! jq -e --arg os "${os}" --arg arch "${arch}" \
      '.manifests // [] | any(.platform.os == $os and .platform.architecture == $arch)' \
      <<<"${raw}" >/dev/null; then
      return 0
    fi
  done
  docker buildx imagetools inspect "$1" --format '{{json .Manifest}}' | jq -r .digest
}

declare -A DIGESTS
complete=true
for image in "${RELEASE_IMAGES[@]}"; do
  digest="$(existing_digest "${AX_IMAGE_REPO}/${image}:${VERSION}")"
  if [[ -z "${digest}" ]]; then
    complete=false
  else
    DIGESTS[${image}]="${digest}"
  fi
done

if [[ "${complete}" == true && "${OVERWRITE}" != true ]]; then
  echo "==> Every release image already exists for ${VERSION}; building nothing."
else
  for p in "${RELEASE_PLATFORMS[@]}"; do
    os="${p%/*}"
    arch="${p#*/}"
    mkdir -p "bin/${os}_${arch}"
    for image in "${RELEASE_IMAGES[@]}"; do
      echo "==> Cross-compiling ${image} for ${p}"
      GOOS="${os}" GOARCH="${arch}" CGO_ENABLED=0 \
        go build -trimpath -ldflags="$(go_ldflags "${VERSION}")" \
        -o "bin/${os}_${arch}/${image}" "./cmd/${image}"
    done
  done

  for image in "${RELEASE_IMAGES[@]}"; do
    ref="${AX_IMAGE_REPO}/${image}:${VERSION}"
    metadata="$(mktemp)"
    echo "==> Building and pushing ${ref} (${PLATFORMS_CSV})"
    docker buildx build \
      --platform "${PLATFORMS_CSV}" \
      --file "Dockerfile.${image#ax-}" \
      --tag "${ref}" \
      --label "org.opencontainers.image.source=https://github.com/google/ax" \
      --label "org.opencontainers.image.version=${VERSION}" \
      --label "org.opencontainers.image.revision=$(git rev-parse HEAD)" \
      --label "org.opencontainers.image.licenses=Apache-2.0" \
      --provenance=false \
      --metadata-file "${metadata}" \
      --push \
      .
    DIGESTS[${image}]="$(jq -r '."containerimage.digest"' "${metadata}")"
    rm -f "${metadata}"
  done
fi

for image in "${RELEASE_IMAGES[@]}"; do
  if [[ ! "${DIGESTS[${image}]:-}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "error: no digest recorded for ${image}" >&2
    exit 1
  fi
done

cat > "${OUT}/images.env" <<EOF
AX_SERVER_IMAGE=${AX_IMAGE_REPO}/ax-server@${DIGESTS[ax-server]}
AX_DEFAULT_TASK_IMAGE=${AX_IMAGE_REPO}/ax-task-runner@${DIGESTS[ax-task-runner]}
EOF
echo "==> Wrote ${OUT}/images.env:"
cat "${OUT}/images.env"
