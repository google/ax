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

# Directory holding the pre-downloaded linux/amd64 wheels baked into the ax image
# (including the google-antigravity wheel that bundles the localharness binary).
# Populate it with `install-ax.sh --fetch-wheels`
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

# wait_with_spinner runs a blocking command while showing a simple spinner on an
# interactive terminal, then reports "done"/"failed" and returns the command's
# exit status.
wait_with_spinner() {
  local msg="$1"; shift
  if [[ ! -t 2 ]]; then
    "$@"
    return $?
  fi

  local out; out="$(mktemp)"
  "$@" >"${out}" 2>&1 &
  local pid=$! frames='|/-\' i=0
  while kill -0 "${pid}" 2>/dev/null; do
    i=$(( (i + 1) % ${#frames} ))
    printf '\r%s %s' "${frames:${i}:1}" "${msg}" >&2
    sleep 0.1
  done

  local status=0
  wait "${pid}" || status=$?
  if [[ "${status}" -eq 0 ]]; then
    printf '\r\033[K%s... done\n' "${msg}" >&2
  else
    printf '\r\033[K%s... failed\n' "${msg}" >&2
    cat "${out}" >&2
  fi
  rm -f "${out}"
  return "${status}"
}

function usage() {
  echo "Usage: $0 [options]"
  echo ""
  echo "Options:"
  echo "  --fetch-wheels                        Download the antigravity wheels into WHEELS_DIR"
  echo "  --deploy-ax-server                    Build images and deploy AX server and components"
  echo "  --delete-ax-server                    Delete AX server and components from cluster"
  echo "  -h, --help                            Show this help message"
}

run_kubectl() {
  kubectl \
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

# build_ax_image builds and pushes the comprehensive ax image (the Go ax binary
# plus the Antigravity Python sidecar) and echoes its digest-pinned reference on
# stdout. Requires KO_DOCKER_REPO, a container engine, and a populated wheel
# cache.
build_ax_image() {
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
  repo="${KO_DOCKER_REPO}/ax"
  tag="$(git rev-parse --short HEAD)"
  image="${repo}:${tag}"

  # The cluster runs on linux/amd64 and the bundled localharness is an amd64
  # binary, so the image must be amd64 regardless of the build host.
  # Relabel multi-stage build step prefixes to friendlier tags matching log_step's
  # style.
  log_step "build_ax_image -> ${image}" >&2
  "${CONTAINER_ENGINE}" build \
    --platform linux/amd64 \
    --build-context "wheels=${WHEELS_DIR}" \
    -f cmd/ax/Dockerfile \
    -t "${image}" \
    . 2>&1 \
    | awk -v cyan="${COLOR_CYAN}" -v reset="${COLOR_RESET}" '
        /^\[[0-9]+\/[0-9]+\] / {
          s = $1; gsub(/[][]/, "", s); split(s, parts, "/")
          stage = (parts[1] == "1") ? "build" : "runtime"
          rest = $0; sub(/^\[[0-9]+\/[0-9]+\] /, "", rest)
          printf "%s[%s]%s %s\n", cyan, stage, reset, rest
          fflush(); next
        }
        { print; fflush() }
      ' >&2

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

build_ateom_image() {
  if [[ -n "${ATEOM_IMAGE:-}" ]]; then
    echo "${ATEOM_IMAGE}"
    return
  fi
  if [[ -z "${KO_DOCKER_REPO:-}" ]]; then
    echo "Error: KO_DOCKER_REPO environment variable must be set" >&2
    exit 1
  fi

  # Resolve the substrate source for the version AX is pinned to in go.mod.
  go mod download github.com/agent-substrate/substrate
  local sub_dir ateom_ref
  sub_dir="$(go list -m -f '{{.Dir}}' github.com/agent-substrate/substrate)"
  if [[ -z "${sub_dir}" ]]; then
    echo "Error: could not locate the substrate module (go list -m)." >&2
    exit 1
  fi

  log_step "build_ateom_image (from ${sub_dir})" >&2
  ateom_ref="$(cd "${sub_dir}" && KO_DOCKER_REPO="${KO_DOCKER_REPO}" GOFLAGS="" ko build --platform=linux/amd64 -B ./cmd/ateom-gvisor)"

  if [[ "${ateom_ref}" != *@sha256:* ]]; then
    echo "Error: ko did not return a digest-pinned ateom image (got '${ateom_ref}')." >&2
    exit 1
  fi
  echo "${ateom_ref}"
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

  # Build and push the images, capturing their digest-pinned references.
  local ax_image ateom_image
  ax_image=$(build_ax_image)
  ateom_image=$(build_ateom_image)

  # Render the manifest and apply it.
  sed -e "s|\${GEMINI_API_KEY}|${GEMINI_API_KEY}|g" \
      -e "s|\${BUCKET_NAME}|${BUCKET_NAME}|g" \
      -e "s|\${AX_IMAGE}|${ax_image}|g" \
      -e "s|\${ATEOM_IMAGE}|${ateom_image}|g" \
      internal/manifests/ax-deployment2.yaml \
      | run_kubectl apply -f -

  # Wait for the antigravity ActorTemplate's golden snapshot to be ready.
  log_step "wait for actortemplate/ax-harness-template to be Ready"
  wait_with_spinner "waiting for golden snapshot (timeout ${AX_WAIT_TIMEOUT:-5m})" \
    run_kubectl wait --for=condition=Ready actortemplate/ax-harness-template \
    -n ax --timeout="${AX_WAIT_TIMEOUT:-5m}"
}

delete_ax_server() {
  log_step "delete_ax_server"

  # Delete resources using dummy values so credentials aren't required for deletion
  sed -e "s|\${GEMINI_API_KEY}|dummy-key|g" \
      -e "s|\${BUCKET_NAME}|dummy-bucket|g" \
      -e "s|\${AX_IMAGE}|dummy-image|g" \
      -e "s|\${ATEOM_IMAGE}|dummy-image|g" \
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
