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

# Builds ax and the AX images from this checkout, pushes the images to a
# registry on localhost, and runs `ax local start` with them: "Using Locally
# Built Images" in docs/local_installer.md. Unlike hack/test-local-install.sh,
# it uses your kubeconfig and ~/.ax and leaves the cluster and the registry
# running. Run it again after a change: an existing cluster only rolls out the
# new ax-server.
#
# Configurable environment variables:
#   AX_LOCAL_CLUSTER  Name of the local cluster (default: ax-local)
#   REGISTRY_PORT     Port of the registry on localhost if it is not running yet (default: 5001)
#   REGISTRY_IMAGE    Image of the registry container
#   CONTAINER_CLI     Container engine for the images and the registry (default: podman, else docker)
#   IMAGE_ARCH        Architecture of the images (default: go env GOARCH)

set -euo pipefail
export GOWORK=off

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

CLUSTER="${AX_LOCAL_CLUSTER:-ax-local}"
REGISTRY_PORT="${REGISTRY_PORT:-5001}"
REGISTRY_IMAGE="${REGISTRY_IMAGE:-registry:3@sha256:82f6ff4a7f982960c8aa51617fea3aabc485df6e55cce80d22deb1de36f1bc3c}"
CONTAINER_CLI="${CONTAINER_CLI:-$(command -v podman || command -v docker || true)}"
IMAGE_ARCH="${IMAGE_ARCH:-$(go env GOARCH)}"
AX="${REPO_ROOT}/bin/ax"

log() {
  printf '\n==> %s\n' "$*" >&2
}

if [[ -z "${CONTAINER_CLI}" ]]; then
  echo "neither podman nor docker found in PATH (or set CONTAINER_CLI)" >&2
  exit 1
fi

# The Agent Substrate that `ax local start` installs names the registry that
# serves the images of localhost to the cluster, and `ax` reads the name from
# the same manifest.
endpoint="$(sed -n 's/.*--localhost-registry-replacement=\([^[:space:]"]*\).*/\1/p' \
  "${REPO_ROOT}/internal/install/substrate/manifests/05-ate-system.yaml" | head -n1)"
if [[ "${endpoint}" != *:* ]]; then
  echo "cannot read the local registry from the embedded Substrate manifests (got '${endpoint}')" >&2
  exit 1
fi
registry_name="${endpoint%%:*}"
registry_container_port="${endpoint##*:}"

if [[ "$("${CONTAINER_CLI}" inspect -f '{{.State.Running}}' "${registry_name}" 2> /dev/null)" == "true" ]]; then
  log "Using the running registry container '${registry_name}'."
elif "${CONTAINER_CLI}" inspect "${registry_name}" > /dev/null 2>&1; then
  log "Starting the stopped registry container '${registry_name}'..."
  "${CONTAINER_CLI}" start "${registry_name}" > /dev/null
else
  log "Starting the registry container '${registry_name}' on localhost:${REGISTRY_PORT}..."
  "${CONTAINER_CLI}" run -d -p "127.0.0.1:${REGISTRY_PORT}:${registry_container_port}" \
    --name "${registry_name}" "${REGISTRY_IMAGE}" > /dev/null
fi
REGISTRY_PORT="$("${CONTAINER_CLI}" port "${registry_name}" "${registry_container_port}/tcp" | head -n1 | sed 's/.*://')"
for _ in $(seq 1 30); do
  curl -fsS -o /dev/null "http://localhost:${REGISTRY_PORT}/v2/" 2> /dev/null && break
  sleep 1
done

log "Building ax and the linux/${IMAGE_ARCH} AX images into localhost:${REGISTRY_PORT}..."
cd "${REPO_ROOT}"
mkdir -p bin .tmp
CGO_ENABLED=0 go build -o "${AX}" ./cmd/ax
make images AX_IMAGE_REPO="localhost:${REGISTRY_PORT}" IMAGE_ARCH="${IMAGE_ARCH}" CONTAINER_CLI="${CONTAINER_CLI}" \
  > .tmp/build-local-cluster-images.log 2>&1 ||
  { tail -n 40 .tmp/build-local-cluster-images.log >&2; exit 1; }
server_image="localhost:${REGISTRY_PORT}/ax-server@$(cat bin/ax-server.digest)"
task_image="localhost:${REGISTRY_PORT}/ax-runner@$(cat bin/ax-runner.digest)"

log "Running ax local start with ${server_image}..."
unset AX_SERVER
"${AX}" local start --name "${CLUSTER}" --server-image "${server_image}"
"${AX}" local status --name "${CLUSTER}"

cat >&2 << EOF

==> Local cluster "${CLUSTER}" runs the AX built from this checkout. Use this
    ax, and run tasks from the matching runner image:

    export PATH="${REPO_ROOT}/bin:\$PATH"
    export AX_TASK_IMAGE="${task_image}"   # spec.profile.image
EOF
