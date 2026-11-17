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
# An image that already exists for <version> on every platform is not rebuilt;
# its digest is recorded as is, so a rerun never changes the digest of an image
# that was pushed. Only missing or incomplete images are built.
# AX_PUBLISH_OVERWRITE=true rebuilds every image.
#
# AX_RELEASE_STATE is the state of the GitHub release for <version>: none
# (default), draft, or published. Once it is published, nothing is built: the
# images must all exist already, or the script fails.
#
# If <version> is the highest stable tag at origin, :latest is moved to it as
# well; dev builds of `ax install` deploy :latest (internal/version).
#
# Needs Go, docker buildx with a builder that can push multi-platform images
# (docker buildx create --driver docker-container --use), QEMU for the
# non-native platform, jq, file, a docker login to the registry, and for
# ghcr.io, gh (GH_TOKEN) to tell a missing package from a denied one.

set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=hack/release/lib.sh
source "${ROOT}/hack/release/lib.sh"

VERSION="${1:-}"
OUT="${2:-${ROOT}/bin/release}"
OVERWRITE="${AX_PUBLISH_OVERWRITE:-false}"
RELEASE_STATE="${AX_RELEASE_STATE:-none}"
require_release_version "${VERSION}"
if [[ -z "${AX_IMAGE_REPO:-}" ]]; then
  echo "error: AX_IMAGE_REPO is not set (e.g. AX_IMAGE_REPO=ghcr.io/google/ax)" >&2
  exit 1
fi
case "${RELEASE_STATE}" in
  none | draft | published) ;;
  *) echo "error: AX_RELEASE_STATE must be none, draft, or published, not '${RELEASE_STATE}'" >&2; exit 1 ;;
esac

mkdir -p "${OUT}"
OUT="$(cd "${OUT}" && pwd)"
cd "${ROOT}"

PLATFORMS_CSV="$(IFS=,; echo "${RELEASE_PLATFORMS[*]}")"

# Release Dockerfiles must take TARGETARCH from buildx. An `ARG TARGETARCH=amd64`
# default (AX before v0.4) copies the amd64 binary into every platform's image.
for image in "${RELEASE_IMAGES[@]}"; do
  if grep -Eq '^ARG TARGETARCH=' "Dockerfile.${image#ax-}"; then
    echo "error: Dockerfile.${image#ax-} pins TARGETARCH, so it cannot build multi-arch release images" >&2
    exit 1
  fi
done

# ghcr_package_missing succeeds if ghcr.io reference $1 names a package that
# does not exist. ghcr.io answers DENIED, not NAME_UNKNOWN, for a package that
# was never pushed, so a denied pull is resolved through the packages API: 404
# means missing; anything else (including an existing package) is an error.
ghcr_package_missing() {
  local path="${1#ghcr.io/}"
  path="${path%%[:@]*}"
  local owner="${path%%/*}" name="${path#*/}"
  local code kind
  for kind in orgs users; do
    code="$(gh api -i "${kind}/${owner}/packages/container/${name//\//%2F}" 2>/dev/null \
      | awk 'NR == 1 { print $2 }' || true)"
    [[ "${code}" == 404 ]] && continue
    return 1
  done
  return 0
}

# existing_digest prints the index digest of $1 and returns 0 if it exists and
# holds an image for every platform in RELEASE_PLATFORMS. It returns 1 if the
# image is missing or lacks a platform. Any other error (auth, rate limit,
# network) returns 2, so a transient failure never rebuilds and replaces a
# published image.
existing_digest() {
  local raw err
  err="$(mktemp)"
  if ! raw="$(docker buildx imagetools inspect --raw "$1" 2>"${err}")"; then
    local msg
    msg="$(cat "${err}")"
    rm -f "${err}"
    if [[ "${msg}" == *": not found"* || "${msg}" == *"MANIFEST_UNKNOWN"* || "${msg}" == *"NAME_UNKNOWN"* ]]; then
      return 1
    fi
    if [[ "$1" == ghcr.io/* && "${msg}" == *"DENIED"* ]] && ghcr_package_missing "$1"; then
      return 1
    fi
    echo "error: cannot check whether $1 exists: ${msg}" >&2
    return 2
  fi
  rm -f "${err}"
  local p
  for p in "${RELEASE_PLATFORMS[@]}"; do
    if ! jq -e --arg os "${p%/*}" --arg arch "${p#*/}" \
      '.manifests // [] | any(.platform.os == $os and .platform.architecture == $arch)' \
      <<<"${raw}" >/dev/null; then
      echo "==> $1 exists but has no ${p} image" >&2
      return 1
    fi
  done
  local manifest
  manifest="$(docker buildx imagetools inspect "$1" --format '{{json .Manifest}}')" || return 2
  jq -er .digest <<<"${manifest}" || return 2
}

declare -A DIGESTS
build=()
for image in "${RELEASE_IMAGES[@]}"; do
  if [[ "${OVERWRITE}" == true ]]; then
    build+=("${image}")
    continue
  fi
  rc=0
  digest="$(existing_digest "${AX_IMAGE_REPO}/${image}:${VERSION}")" || rc=$?
  case "${rc}" in
    0)
      DIGESTS[${image}]="${digest}"
      echo "==> ${AX_IMAGE_REPO}/${image}:${VERSION} exists (${digest}); not rebuilding"
      ;;
    1) build+=("${image}") ;;
    *) exit 1 ;;
  esac
done

if [[ ${#build[@]} -gt 0 && "${RELEASE_STATE}" == published ]]; then
  echo "error: release ${VERSION} is published; refusing to build ${build[*]}" >&2
  exit 1
fi

if [[ ${#build[@]} -gt 0 ]]; then
  revision="$(git rev-parse HEAD)"
  for p in "${RELEASE_PLATFORMS[@]}"; do
    os="${p%/*}"
    arch="${p#*/}"
    mkdir -p "bin/${os}_${arch}"
    for image in "${build[@]}"; do
      echo "==> Cross-compiling ${image} for ${p}"
      GOOS="${os}" GOARCH="${arch}" CGO_ENABLED=0 \
        go build -trimpath -ldflags="$(go_ldflags "${VERSION}")" \
        -o "bin/${os}_${arch}/${image}" "./cmd/${image}"
    done
  done

  for image in "${build[@]}"; do
    ref="${AX_IMAGE_REPO}/${image}:${VERSION}"
    metadata="$(mktemp)"
    echo "==> Building and pushing ${ref} (${PLATFORMS_CSV})"
    # GHCR reads the source of a multi-platform image from the index
    # annotation, which links the package to the repository.
    docker buildx build \
      --platform "${PLATFORMS_CSV}" \
      --file "Dockerfile.${image#ax-}" \
      --tag "${ref}" \
      --label "org.opencontainers.image.source=https://github.com/google/ax" \
      --label "org.opencontainers.image.version=${VERSION}" \
      --label "org.opencontainers.image.revision=${revision}" \
      --label "org.opencontainers.image.licenses=Apache-2.0" \
      --annotation "index:org.opencontainers.image.source=https://github.com/google/ax" \
      --annotation "index:org.opencontainers.image.version=${VERSION}" \
      --annotation "index:org.opencontainers.image.revision=${revision}" \
      --annotation "index:org.opencontainers.image.licenses=Apache-2.0" \
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

# Each platform's image must hold that platform's binary. Checked on existing
# images too, so a rerun never records a broken set.
for image in "${RELEASE_IMAGES[@]}"; do
  for p in "${RELEASE_PLATFORMS[@]}"; do
    case "${p#*/}" in
      amd64) want=x86-64 ;;
      arm64) want=aarch64 ;;
      *) echo "error: no binary check for ${p}" >&2; exit 1 ;;
    esac
    cid="$(docker create --platform "${p}" "${AX_IMAGE_REPO}/${image}@${DIGESTS[${image}]}")"
    got="$(docker cp "${cid}:/usr/local/bin/${image}" - | tar -xO | file -b -)"
    docker rm "${cid}" >/dev/null
    if [[ "${got}" != *"${want}"* ]]; then
      echo "error: ${image} for ${p} holds the wrong binary: ${got}" >&2
      exit 1
    fi
    echo "==> ${image} ${p}: ${want} binary"
  done
done

# Point :latest at the newest stable release, from the recorded digests so a
# rerun that built nothing still moves it. Tags are refetched first, so a
# release that started before a newer tag was pushed does not take :latest
# back from it.
if ! is_prerelease "${VERSION}"; then
  git fetch --quiet --tags --force origin
  if is_latest_stable "${VERSION}"; then
    for image in "${RELEASE_IMAGES[@]}"; do
      echo "==> Tagging ${AX_IMAGE_REPO}/${image}:latest -> ${VERSION}"
      docker buildx imagetools create \
        --tag "${AX_IMAGE_REPO}/${image}:latest" \
        "${AX_IMAGE_REPO}/${image}@${DIGESTS[${image}]}"
    done
  fi
fi

cat > "${OUT}/images.env" <<EOF
AX_SERVER_IMAGE=${AX_IMAGE_REPO}/ax-server@${DIGESTS[ax-server]}
AX_DEFAULT_TASK_IMAGE=${AX_IMAGE_REPO}/ax-task-runner@${DIGESTS[ax-task-runner]}
EOF
echo "==> Wrote ${OUT}/images.env:"
cat "${OUT}/images.env"
