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

# Smoke test of the local installer. It does what "Using Locally Built Images"
# in docs/local_installer.md describes: it builds the AX images into a registry
# on localhost, runs `ax local start` with them, and runs a task from a locally
# built image. The E2E suite installs AX with Substrate's own tools
# (hack/setup-e2e-kind.sh), so this is the only test that runs `ax local start`
# on a real cluster.
#
# The cluster gets its own kubeconfig and AX home below .tmp, so the test does
# not change ~/.kube/config or ~/.ax. On exit it deletes the cluster, and the
# registry container if it started one.
#
# Configurable environment variables:
#   AX_LOCAL_CLUSTER  Name of the local cluster (default: ax-smoke)
#   REGISTRY_PORT     Port of the registry on localhost (default: 5001)
#   REGISTRY_IMAGE    Image of the registry container
#   CONTAINER_CLI     Container engine for the images, the registry and the cluster (default: docker)
#   KEEP_CLUSTER      Set to true to leave the cluster and the registry running

set -euo pipefail
export GOWORK=off

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
TMP_DIR="${REPO_ROOT}/.tmp"
mkdir -p "${TMP_DIR}/bin"

CLUSTER="${AX_LOCAL_CLUSTER:-ax-smoke}"
REGISTRY_PORT="${REGISTRY_PORT:-5001}"
REGISTRY_IMAGE="${REGISTRY_IMAGE:-registry:3@sha256:82f6ff4a7f982960c8aa51617fea3aabc485df6e55cce80d22deb1de36f1bc3c}"
export CONTAINER_CLI="${CONTAINER_CLI:-docker}"
export KUBECONFIG="${TMP_DIR}/kubeconfig-${CLUSTER}"
export AX_HOME="${TMP_DIR}/ax-home-${CLUSTER}"
unset AX_SERVER AX_SERVER_IMAGE AX_TASK_RUNNER_IMAGE AX_RUNNER_VOLUME_IMAGE AX_RUNNER_VOLUME_BUILD_IMAGE

AX="${TMP_DIR}/bin/ax"
TASK="local-smoke"
started_registry=false

log() {
  printf '\n==> %s\n' "$*" >&2
}

cleanup() {
  local status=$?
  if ((status != 0)); then
    log "Failed. State of the cluster:"
    kubectl get pods -A -o wide >&2 || true
    kubectl -n ax-system logs deployment/ax-server --tail=100 >&2 || true
  fi
  if [[ "${KEEP_CLUSTER:-false}" != "true" ]]; then
    if [[ -x "${AX}" ]]; then
      "${AX}" local delete --name "${CLUSTER}" > /dev/null 2>&1 || true
    fi
    if [[ "${started_registry}" == "true" ]]; then
      "${CONTAINER_CLI}" rm -f -v "${registry_name}" > /dev/null 2>&1 || true
    fi
  fi
  exit "${status}"
}
trap cleanup EXIT

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
  REGISTRY_PORT="$("${CONTAINER_CLI}" port "${registry_name}" "${registry_container_port}/tcp" | head -n1 | sed 's/.*://')"
  log "Using the running registry container '${registry_name}' on localhost:${REGISTRY_PORT}."
else
  log "Starting the registry container '${registry_name}' on localhost:${REGISTRY_PORT}..."
  "${CONTAINER_CLI}" run -d -p "127.0.0.1:${REGISTRY_PORT}:${registry_container_port}" \
    --name "${registry_name}" "${REGISTRY_IMAGE}" > /dev/null
  started_registry=true
fi
for _ in $(seq 1 30); do
  curl -fsS -o /dev/null "http://localhost:${REGISTRY_PORT}/v2/" 2> /dev/null && break
  sleep 1
done

log "Building ax and the AX images into localhost:${REGISTRY_PORT}..."
(
  cd "${REPO_ROOT}"
  CGO_ENABLED=0 go build -o "${AX}" ./cmd/ax
  make images AX_IMAGE_REPO="localhost:${REGISTRY_PORT}" IMAGE_ARCH="$(go env GOARCH)" > "${TMP_DIR}/local-install-images.log" 2>&1 ||
    { tail -n 40 "${TMP_DIR}/local-install-images.log" >&2; exit 1; }
)
server_image="localhost:${REGISTRY_PORT}/ax-server@$(cat "${REPO_ROOT}/bin/ax-server.digest")"
task_image="localhost:${REGISTRY_PORT}/ax-task-runner@$(cat "${REPO_ROOT}/bin/ax-task-runner.digest")"

log "Running ax local start with ${server_image}..."
"${AX}" local start --name "${CLUSTER}" --server-image "${server_image}"
"${AX}" local status --name "${CLUSTER}"

log "Running a task from ${task_image}..."
"${AX}" apply -f - << EOF
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: ${TASK}
  atespace: default
spec:
  debug: true
  profile:
    image: "${task_image}"
  commands: ["sleep", "infinity"]
  env:
    - name: GREETING
      value: "hello-from-ax"
EOF
"${AX}" resume task "${TASK}"
# The sandbox expands $GREETING, not this shell.
# shellcheck disable=SC2016
got="$("${AX}" ssh "${TASK}" -- sh -c 'echo "ENV=$GREETING"' | tr -d '\r')"
if [[ "${got}" != "ENV=hello-from-ax" ]]; then
  echo "ax ssh ${TASK} printed '${got}', want 'ENV=hello-from-ax'" >&2
  exit 1
fi
"${AX}" delete task "${TASK}"

log "The local install works: ax local start, and a task from a locally built image."
