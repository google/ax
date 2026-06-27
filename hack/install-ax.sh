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

# Source developer environment overrides if present. Copy
# hack/ax-dev-env.sh.example to .ax-dev-env.sh to configure your deployment.
# Set NO_DEV_ENV=1 to skip.
if [[ -f .ax-dev-env.sh ]] && [[ -z "${NO_DEV_ENV:-}" ]]; then
  # shellcheck source=/dev/null
  source .ax-dev-env.sh
fi

if [[ -n "${PROJECT_ID:-}" ]]; then
  # Default the image repo to the project's GCR, but respect an explicit
  # override (e.g. from .ax-dev-env.sh).
  export KO_DOCKER_REPO="${KO_DOCKER_REPO:-gcr.io/${PROJECT_ID}}"
  echo "Using KO_DOCKER_REPO: ${KO_DOCKER_REPO}" >&2
fi

export KO_DEFAULTPLATFORMS="${KO_DEFAULTPLATFORMS:-linux/amd64}"

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
  echo "Deploys Agent Substrate and AX together. The substrate version is taken"
  echo "automatically from AX's go.mod pin; you never choose a substrate commit."
  echo ""
  echo "One-time cluster setup (run once per cluster):"
  echo ""
  echo "  --create-cluster                      Provision GCP resources (GKE cluster, GCS bucket, IAM)"
  echo "  --delete-cluster                      Tear down the provisioned GCP resources"
  echo ""
  echo "Deploy (re-run as you update your code):"
  echo ""
  echo "  --deploy-all                          Deploy the substrate control plane and the AX server"
  echo "  --delete-all                          Delete the AX server and the substrate control plane"
  echo ""
  echo "Granular components:"
  echo ""
  echo "  --deploy-ate-system                   Deploy the substrate control plane (at AX's pinned version)"
  echo "  --delete-ate-system                   Delete the substrate control plane"
  echo "  --deploy-ax-server                    Build images and deploy AX server and components"
  echo "  --delete-ax-server                    Delete AX server and components, preserving the event-log database"
  echo ""
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

# build_ax_image builds and pushes the comprehensive ax image (the Go ax binary
# plus the Antigravity Python sidecar) and echoes its digest-pinned reference on
# stdout. Requires KO_DOCKER_REPO and a container engine.
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

  local repo tag image digest
  repo="${KO_DOCKER_REPO}/ax"
  tag="$(git rev-parse --short HEAD)"
  image="${repo}:${tag}"

  log_step "build_ax_image -> ${image}" >&2
  "${CONTAINER_ENGINE}" build \
    --platform linux/amd64 \
    -f cmd/ax/Dockerfile \
    -t "${image}" \
    . 2>&1 \
    | awk '{ sub(/^\[[0-9]+\/[0-9]+\] /, ""); print; fflush() }' >&2

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

# --- Agent Substrate (compute layer) ---------------------------------------
#
# AX runs on Agent Substrate. The substrate version is taken automatically from
# AX's go.mod pin, so developers never choose a substrate commit.

# require_env exits with a clear message if any named environment variable is
# unset or empty.
require_env() {
  local missing=() v
  for v in "$@"; do
    [[ -n "${!v:-}" ]] || missing+=("${v}")
  done
  if [[ "${#missing[@]}" -gt 0 ]]; then
    echo "Error: missing required environment variables: ${missing[*]}" >&2
    echo "Copy hack/ax-dev-env.sh.example to .ax-dev-env.sh, fill it in, and re-run." >&2
    exit 1
  fi
}

# ensure_substrate_src materializes a git checkout of Agent Substrate at the
# exact version AX pins in go.mod and echoes its directory on stdout. By default
# it maintains a managed clone under the user cache; set AX_SUBSTRATE_DIR to use
# your own substrate checkout instead.
ensure_substrate_src() {
  local commit
  commit="$(go list -m -f '{{.Version}}' github.com/agent-substrate/substrate | sed 's/.*-//')"
  if [[ -z "${commit}" ]]; then
    echo "Error: could not determine the pinned substrate version from go.mod." >&2
    exit 1
  fi

  local dir="${AX_SUBSTRATE_DIR:-${XDG_CACHE_HOME:-${HOME}/.cache}/ax/substrate}"

  if [[ ! -d "${dir}/.git" ]]; then
    if [[ -n "${AX_SUBSTRATE_DIR:-}" ]]; then
      echo "Error: AX_SUBSTRATE_DIR=${AX_SUBSTRATE_DIR} is not a git checkout." >&2
      exit 1
    fi
    log_step "clone substrate -> ${dir}" >&2
    mkdir -p "$(dirname "${dir}")"
    git clone --quiet https://github.com/agent-substrate/substrate "${dir}" >&2
  fi

  # Never clobber uncommitted work in a developer-provided checkout.
  if [[ -n "${AX_SUBSTRATE_DIR:-}" ]] && [[ -n "$(git -C "${dir}" status --porcelain)" ]]; then
    echo "Error: ${dir} has uncommitted changes; refusing to check out ${commit}." >&2
    echo "Commit or stash them, or unset AX_SUBSTRATE_DIR." >&2
    exit 1
  fi

  # Ensure the pinned commit is present locally, then check it out (detached).
  if ! git -C "${dir}" cat-file -e "${commit}^{commit}" 2>/dev/null; then
    log_step "fetch substrate ${commit}" >&2
    git -C "${dir}" fetch --quiet origin >&2 || true
  fi
  if ! git -C "${dir}" cat-file -e "${commit}^{commit}" 2>/dev/null; then
    git -C "${dir}" fetch --quiet origin "${commit}" >&2 \
      || { echo "Error: could not fetch substrate commit ${commit}." >&2; exit 1; }
  fi
  git -C "${dir}" checkout --quiet --detach "${commit}" >&2

  echo "${dir}"
}

# run_substrate runs a substrate hack/ script from the substrate checkout, in a
# subshell with the checkout as the working directory so substrate's own
# `git rev-parse --show-toplevel` resolves to the substrate tree (not AX's). It
# forwards the current kube context so substrate targets the same cluster.
run_substrate() {
  local src
  src="$(ensure_substrate_src)"
  ( cd "${src}" \
    && KUBECTL_CONTEXT="${KUBECTL_CONTEXT:-$(kubectl config current-context 2>/dev/null || true)}" \
       "$@" )
}

# create_cluster provisions the GCP resources substrate needs (GKE cluster with
# Workload Identity and the required beta APIs, the GCS snapshot bucket, and IAM
# bindings) via substrate's setup-gcp tool. This is a one-time step per cluster.
create_cluster() {
  log_step "create_cluster"
  require_env PROJECT_ID PROJECT_NUMBER CLUSTER_NAME CLUSTER_LOCATION CLUSTER_VERSION \
    NETWORK SUBNETWORK NODE_POOL_NAME NODE_POOL_VERSION GCE_REGION BUCKET_NAME \
    GVISOR_NODE_MACHINE_TYPE
  local src
  src="$(ensure_substrate_src)"
  ( cd "${src}" && go run ./tools/setup-gcp --all )
}

# delete_cluster tears down the GCP resources created by create_cluster.
delete_cluster() {
  log_step "delete_cluster"
  local src
  src="$(ensure_substrate_src)"
  ( cd "${src}" && ./hack/teardown.sh --all )
}

# deploy_ate_system deploys the substrate control plane (CRDs, ateapi,
# atecontroller, atelet, atenet, valkey, pod-cert controller) at AX's pinned
# version. Idempotent: re-applying an unchanged version is a no-op.
deploy_ate_system() {
  log_step "deploy_ate_system"
  run_substrate ./hack/install-ate.sh --deploy-ate-system
}

# delete_ate_system removes the substrate control plane.
delete_ate_system() {
  log_step "delete_ate_system"
  run_substrate ./hack/install-ate.sh --delete-ate-system
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

  # Resolve a stable Postgres password for the event log.
  local pg_password="${POSTGRES_PASSWORD:-}"
  local existing_pw
  existing_pw="$(run_kubectl -n ax get secret ax-eventlog-postgres -o go-template='{{.data.password | base64decode}}' 2>/dev/null || true)"
  if [[ -n "${existing_pw}" ]]; then
    pg_password="${existing_pw}"
  elif [[ -z "${pg_password}" ]]; then
    pg_password="$(openssl rand -hex 16)"
  fi

  # Render the manifest and apply it.
  if ! sed -e "s|\${GEMINI_API_KEY}|${GEMINI_API_KEY}|g" \
      -e "s|\${BUCKET_NAME}|${BUCKET_NAME}|g" \
      -e "s|\${AX_IMAGE}|${ax_image}|g" \
      -e "s|\${ATEOM_IMAGE}|${ateom_image}|g" \
      -e "s|\${POSTGRES_PASSWORD}|${pg_password}|g" \
      manifests/ax-deployment.yaml \
      | run_kubectl apply -f -; then
    echo >&2
    echo "Error: cluster rejected the manifest. An \"unknown field\" error usually means the" >&2
    echo "cluster's substrate is incompatible with AX's go.mod pin. Sync the control" >&2
    echo "plane to the pinned version with: $0 --deploy-ate-system" >&2
    exit 1
  fi

  # Wait for the event-log Postgres to be ready before ax-server relies on it.
  log_step "wait for statefulset/ax-eventlog-postgres to be ready"
  wait_with_spinner "waiting for postgres (timeout ${AX_WAIT_TIMEOUT:-5m})" \
    run_kubectl -n ax rollout status statefulset/ax-eventlog-postgres \
    --timeout="${AX_WAIT_TIMEOUT:-5m}"

  # Wait for the antigravity ActorTemplate's golden snapshot to be ready.
  log_step "wait for actortemplate/ax-harness-template to be Ready"
  wait_with_spinner "waiting for golden snapshot (timeout ${AX_WAIT_TIMEOUT:-5m})" \
    run_kubectl wait --for=condition=Ready actortemplate/ax-harness-template \
    -n ax --timeout="${AX_WAIT_TIMEOUT:-5m}"
}

# delete_ax_server removes the AX server and harness resources but preserves the
# event-log database: it leaves the namespace and the Postgres subsystem
# (Service/Secret/StatefulSet and its PVC) intact so a later redeploy reuses the
# existing data.
delete_ax_server() {
  log_step "delete_ax_server"

  run_kubectl -n ax delete --ignore-not-found \
    replicaset/ax-server \
    configmap/ax-server-config \
    actortemplate/ax-harness-template \
    workerpool/ax-harness-workerpool
}

# deploy_all deploys the substrate control plane and then the AX server.
deploy_all() {
  deploy_ate_system
  deploy_ax_server
}

# delete_all removes the AX server and then the substrate control plane. It does
# not delete the cluster or GCP resources (use --delete-cluster for that).
delete_all() {
  delete_ax_server
  delete_ate_system
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

# PROJECT_ID and CLUSTER_NAME are required for all operations.
require_env PROJECT_ID CLUSTER_NAME

while [[ "$#" -gt 0 ]]; do
  case $1 in
    --create-cluster) create_cluster ;;
    --delete-cluster) delete_cluster ;;
    --deploy-all) deploy_all ;;
    --delete-all) delete_all ;;
    --deploy-ate-system) deploy_ate_system ;;
    --delete-ate-system) delete_ate_system ;;
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
