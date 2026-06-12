#!/bin/bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -e
set -u
set -o pipefail

ROOT=$(git rev-parse --show-toplevel)
cd "${ROOT}"

# Directory holding the pre-downloaded linux/amd64 wheels used to build the
# antigravity harness image (including the google-antigravity wheel that bundles
# the localharness binary). Populate it with `install-ax.sh --fetch-wheels`
# (delete the directory first to refresh from scratch). Override the location
# with the WHEELS_DIR env var.
WHEELS_DIR="${WHEELS_DIR:-${HOME}/.cache/ax-antigravity-wheels}"

# Python interpreter used by --fetch-wheels. Any Python 3 with pip works; the
# downloaded wheels always target the container's Python (3.13) regardless of
# this interpreter's own version. Override with the PYTHON env var.
PYTHON="${PYTHON:-python3}"

# ANSI color codes for prettier output
COLOR_CYAN='\033[1;36m'
COLOR_RESET='\033[0m'

function log_step() {
  local step_name="$1"
  echo -e "${COLOR_CYAN}[step]: ${step_name}${COLOR_RESET}"
}

function usage() {
  echo "Usage: $0 [options]"
  echo ""
  echo "Options:"
  echo "  --fetch-wheels                        Download the antigravity harness wheels into WHEELS_DIR"
  echo "  --deploy-ax-server                    Deploy AX server and components using ko"
  echo "  --delete-ax-server                    Delete AX server and components from cluster"
  echo "  -h, --help                            Show this help message"
}

run_kubectl() {
  kubectl \
    ${KUBECTL_CONTEXT:+--context=${KUBECTL_CONTEXT}} \
    "$@"
}

run_ko() {
  GOFLAGS="-tags=harness" ko apply \
    ${KUBECTL_CONTEXT:+--context=${KUBECTL_CONTEXT}} \
    "$@"
}

# detect_container_engine selects the OCI build/push tool when CONTAINER_ENGINE
# is not set explicitly. It prefers a *working* docker (daemon reachable), then a
# working podman, so a docker CLI installed without a running daemon does not
# shadow a working podman. As a last resort it picks whichever CLI exists so the
# build step can surface an actionable daemon error.
detect_container_engine() {
  if [[ -n "${CONTAINER_ENGINE:-}" ]]; then
    return  # Respect an explicit override; do not second-guess it.
  fi
  if docker info >/dev/null 2>&1; then
    CONTAINER_ENGINE=docker
  elif podman info >/dev/null 2>&1; then
    CONTAINER_ENGINE=podman
  elif command -v docker >/dev/null 2>&1; then
    CONTAINER_ENGINE=docker
  elif command -v podman >/dev/null 2>&1; then
    CONTAINER_ENGINE=podman
  else
    CONTAINER_ENGINE=docker
  fi
}

# fetch_wheels downloads the antigravity harness wheel closure into WHEELS_DIR.
# Resolution targets linux/amd64 + CPython 3.13 regardless of the host OS or host
# Python.
fetch_wheels() {
  log_step "fetch_wheels -> ${WHEELS_DIR}"

  if ! "${PYTHON}" -m pip --version >/dev/null 2>&1; then
    echo "Error: '${PYTHON} -m pip' is not available." >&2
    echo "Install pip or set PYTHON to an interpreter that has it." >&2
    exit 1
  fi

  mkdir -p "${WHEELS_DIR}"

  "${PYTHON}" -m pip download \
    --only-binary=:all: \
    --python-version 3.13 \
    --platform manylinux_2_17_x86_64 \
    --platform manylinux2014_x86_64 \
    --platform manylinux_2_28_x86_64 \
    --platform manylinux1_x86_64 \
    --platform linux_x86_64 \
    -r python/antigravity/requirements.txt \
    -d "${WHEELS_DIR}"

  echo "Wheel cache ready: ${WHEELS_DIR}"
}

# build_antigravity_image builds and pushes the antigravity harness image and
# echoes its digest-pinned reference on stdout. Requires KO_DOCKER_REPO,
# a container engine, and a populated wheel cache.
build_antigravity_image() {
  if [[ -z "${KO_DOCKER_REPO:-}" ]]; then
    echo "Error: KO_DOCKER_REPO environment variable must be set" >&2
    exit 1
  fi
  detect_container_engine
  if ! command -v "${CONTAINER_ENGINE}" >/dev/null 2>&1; then
    echo "Error: container engine '${CONTAINER_ENGINE}' not found in PATH." >&2
    echo "Install it or set CONTAINER_ENGINE to an available builder." >&2
    exit 1
  fi
  if [[ ! -d "${WHEELS_DIR}" ]] || [[ -z "$(ls -A "${WHEELS_DIR}" 2>/dev/null)" ]]; then
    echo "Error: antigravity wheel cache '${WHEELS_DIR}' is missing or empty." >&2
    echo "Run '$0 --fetch-wheels' to populate it (or set WHEELS_DIR)." >&2
    exit 1
  fi

  local repo tag image digest
  repo="${KO_DOCKER_REPO}/ax-antigravity-harness"
  tag="$(git rev-parse --short HEAD)"
  image="${repo}:${tag}"

  # The cluster runs on linux/amd64 and the bundled localharness is an amd64
  # binary, so the image must be amd64 regardless of the build host.
  log_step "build_antigravity_image -> ${image}" >&2
  "${CONTAINER_ENGINE}" build \
    --platform linux/amd64 \
    --build-context "wheels=${WHEELS_DIR}" \
    -f python/antigravity/Dockerfile \
    -t "${image}" \
    . >&2

  # Push the readable tag, then resolve the pushed manifest digest so the
  # ActorTemplate can reference the image by digest (snapshot-safe).
  if [[ "${CONTAINER_ENGINE}" == *podman* ]]; then
    local digestfile
    digestfile="$(mktemp)"
    "${CONTAINER_ENGINE}" push --digestfile="${digestfile}" "${image}" >&2
    digest="$(cat "${digestfile}")"
    rm -f "${digestfile}"
  else
    "${CONTAINER_ENGINE}" push "${image}" >&2
    local repo_digest
    repo_digest="$("${CONTAINER_ENGINE}" image inspect --format '{{index .RepoDigests 0}}' "${image}")"
    digest="${repo_digest##*@}"
  fi

  if [[ "${digest}" != sha256:* ]]; then
    echo "Error: could not resolve a sha256 digest for ${image} (got '${digest}')." >&2
    exit 1
  fi

  echo "${repo}@${digest}"
}

deploy_ax_server() {
  log_step "deploy_ax_server"

  # Check dependencies
  if [[ -z "${GEMINI_API_KEY:-}" ]]; then
    echo "Error: GEMINI_API_KEY environment variable must be set" >&2
    exit 1
  fi
  if [[ -z "${BUCKET_NAME:-}" ]]; then
    echo "Error: BUCKET_NAME environment variable must be set" >&2
    exit 1
  fi

  echo "Using GCS Bucket: ${BUCKET_NAME}"

  # Build and push the antigravity harness image, capturing its reference.
  local antigravity_image
  antigravity_image=$(build_antigravity_image)

  # Render template and apply with ko
  sed -e "s|\${GEMINI_API_KEY}|${GEMINI_API_KEY}|g" \
      -e "s|\${BUCKET_NAME}|${BUCKET_NAME}|g" \
      -e "s|\${ANTIGRAVITY_IMAGE}|${antigravity_image}|g" \
      internal/manifests/ax-deployment2.yaml \
      | run_ko -f -
}

delete_ax_server() {
  log_step "delete_ax_server"

  # Delete resources using dummy values so credentials aren't required for deletion
  sed -e "s|\${GEMINI_API_KEY}|dummy-key|g" \
      -e "s|\${BUCKET_NAME}|dummy-bucket|g" \
      -e "s|\${ANTIGRAVITY_IMAGE}|dummy-image|g" \
      internal/manifests/ax-deployment2.yaml \
      | run_kubectl delete --ignore-not-found -f -
}

if [ "$#" -eq 0 ]; then
  usage
  exit 1
fi

# If -h or --help appears anywhere in the command line, print the usage and exit.
for arg in "$@"; do
  case "$arg" in
    -h|--help)
      usage
      exit 0
      ;;
  esac
done

while [[ "$#" -gt 0 ]]; do
  case $1 in
    --fetch-wheels) fetch_wheels ;;
    --deploy-ax-server) deploy_ax_server ;;
    --delete-ax-server) delete_ax_server ;;
    *)
      echo "Error: unknown option: $1" >&2
      echo ""
      usage
      exit 1
      ;;
  esac
  shift
done
