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

# Cross-compiles ax-server and ax-task-runner for linux/amd64 and linux/arm64,
# then builds and pushes multi-arch OCI Image Indexes via `docker buildx`.
#
# Usage:
#   AX_IMAGE_REPO=ghcr.io/<github-user>/ax IMAGE_TAG=latest ./hack/publish-multiarch-images.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

AX_IMAGE_REPO="${AX_IMAGE_REPO:-}"
IMAGE_TAG="${IMAGE_TAG:-latest}"
PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64}"
BUILDER_NAME="${BUILDER_NAME:-ax-multiarch}"

if [[ -z "${AX_IMAGE_REPO}" ]]; then
  echo "error: AX_IMAGE_REPO is required (e.g. AX_IMAGE_REPO=ghcr.io/google/ax)" >&2
  exit 1
fi
AX_IMAGE_REPO="${AX_IMAGE_REPO%/}"

cd "${REPO_ROOT}"
export GOTOOLCHAIN=auto

for arch in amd64 arm64; do
  echo "==> Cross-compiling ax-server and ax-task-runner for linux/${arch}..."
  mkdir -p "bin/linux_${arch}"
  CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" go build -trimpath -ldflags="-s -w" \
    -o "bin/linux_${arch}/ax-server" ./cmd/ax-server
  CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" go build -trimpath -ldflags="-s -w" \
    -o "bin/linux_${arch}/ax-task-runner" ./cmd/ax-task-runner
done

# Ensure QEMU binfmt is registered on x86_64/arm64 Linux hosts so cross-arch
# Dockerfile RUN steps (such as apt-get in Dockerfile.task-runner) succeed.
if [[ "$(uname -s)" == "Linux" ]]; then
  if ! [[ -f /proc/sys/fs/binfmt_misc/qemu-aarch64 && -f /proc/sys/fs/binfmt_misc/qemu-x86_64 ]]; then
    echo "==> Registering QEMU binfmt handlers for cross-architecture RUN steps..."
    docker run --privileged --rm tonistiigi/binfmt --install all >/dev/null
  fi
fi

# The default `docker` driver cannot push multi-arch manifest lists unless
# containerd-snapshotter is enabled; ensure a `docker-container` buildx builder.
if ! docker buildx inspect "${BUILDER_NAME}" >/dev/null 2>&1; then
  echo "==> Creating buildx builder ${BUILDER_NAME} (docker-container driver)..."
  docker buildx create --name "${BUILDER_NAME}" --driver docker-container --bootstrap >/dev/null
else
  docker buildx inspect --bootstrap "${BUILDER_NAME}" >/dev/null
fi

SERVER_REF="${AX_IMAGE_REPO}/ax-server:${IMAGE_TAG}"
RUNNER_REF="${AX_IMAGE_REPO}/ax-task-runner:${IMAGE_TAG}"

echo "==> Building and pushing multi-arch (${PLATFORMS}) ${SERVER_REF}..."
docker buildx build \
  --builder "${BUILDER_NAME}" \
  --platform "${PLATFORMS}" \
  -f Dockerfile.server \
  -t "${SERVER_REF}" \
  --push \
  .

echo "==> Building and pushing multi-arch (${PLATFORMS}) ${RUNNER_REF}..."
docker buildx build \
  --builder "${BUILDER_NAME}" \
  --platform "${PLATFORMS}" \
  -f Dockerfile.task-runner \
  -t "${RUNNER_REF}" \
  --push \
  .

echo ""
echo "==> Verifying pushed multi-arch manifests:"
docker buildx imagetools inspect "${SERVER_REF}" | grep -E 'Name:|MediaType:|Digest:|Platform:'
echo "---"
docker buildx imagetools inspect "${RUNNER_REF}" | grep -E 'Name:|MediaType:|Digest:|Platform:'
echo ""
echo "==> Done! Remember to set package visibility to Public if pushing to GHCR for the first time."
