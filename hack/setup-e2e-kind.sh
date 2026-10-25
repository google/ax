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

# Sets up a Kind cluster with Substrate (at a configurable branch/ref) and AX
# deployed for end-to-end testing. Re-running the script reuses the existing
# cluster and only reinstalls Substrate when the requested commit changes.
#
# Configurable environment variables:
#   SUBSTRATE_REF            Branch, tag, commit, or Go module version of substrate (default: version in go.mod)
#   SUBSTRATE_REPO           Git repository URL for substrate (default: https://github.com/agent-substrate/substrate.git)
#   SUBSTRATE_DIR            Existing substrate checkout (default: ../substrate if present, else clone to .tmp/substrate)
#   KIND_CLUSTER_NAME        Name of the Kind cluster (default: ax-e2e)
#   KUBECONFIG               Path to kubeconfig file (default: .tmp/kubeconfig-${KIND_CLUSTER_NAME})
#   AX_SYSTEM_NAMESPACE      Kubernetes namespace for AX control plane (default: ax-system)
#   AX_WORKER_REPLICAS       Number of gVisor worker replicas in the AX WorkerPool (default: 4)
#   AX_SNAPSHOTS_BUCKET      Snapshot storage bucket URI (default: gs://ate-snapshots/ax/)
#   REGISTRY_ADDR            Local container registry address (default: localhost:5001)
#   RECREATE_KIND            Set to true to delete and recreate the Kind cluster
#   FORCE_INSTALL_SUBSTRATE  Set to true to reinstall Substrate even if the commit is unchanged

set -euo pipefail
export GOWORK=off

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
TMP_DIR="${REPO_ROOT}/.tmp"
E2E_DIR="${REPO_ROOT}/test/e2e"
mkdir -p "${TMP_DIR}/bin"

export KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-ax-e2e}"
export KUBECONFIG="${KUBECONFIG:-${TMP_DIR}/kubeconfig-${KIND_CLUSTER_NAME}}"
export KUBE_CONTEXT="kind-${KIND_CLUSTER_NAME}"
export AX_SYSTEM_NAMESPACE="${AX_SYSTEM_NAMESPACE:-ax-system}"
export AX_WORKER_REPLICAS="${AX_WORKER_REPLICAS:-4}"
export AX_SNAPSHOTS_BUCKET="${AX_SNAPSHOTS_BUCKET:-gs://ate-snapshots/ax/}"
export REGISTRY_ADDR="${REGISTRY_ADDR:-localhost:5001}"
SUBSTRATE_REPO="${SUBSTRATE_REPO:-https://github.com/agent-substrate/substrate.git}"
GOARCH="$(go env GOARCH)"

# Default to the Substrate version AX is built against. Go pseudo-versions
SUBSTRATE_REF="${SUBSTRATE_REF:-$(cd "${REPO_ROOT}" && go list -m -f '{{.Version}}' github.com/agent-substrate/substrate)}"
if [[ "${SUBSTRATE_REF}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+-.*-([0-9a-f]{12})$ ]]; then
  SUBSTRATE_REF="${BASH_REMATCH[1]}"
fi

# Use an isolated DOCKER_CONFIG by default so expired gcloud credential helpers
# in ~/.docker/config.json do not break pulling public base images or pushing
# to the local registry.
if [[ -z "${DOCKER_CONFIG:-}" ]]; then
  export DOCKER_CONFIG="${TMP_DIR}/docker-config"
  mkdir -p "${DOCKER_CONFIG}"
  [[ -f "${DOCKER_CONFIG}/config.json" ]] || printf '{}\n' > "${DOCKER_CONFIG}/config.json"
fi

log() {
  printf '\n==> %s\n' "$*" >&2
}

k() {
  kubectl --kubeconfig "${KUBECONFIG}" --context "${KUBE_CONTEXT}" "$@"
}

# render_manifest substitutes only the AX_* variables below so any other `$`
# in the manifests is left untouched.
MANIFEST_VARS=(AX_SYSTEM_NAMESPACE AX_WORKER_REPLICAS AX_SERVER_IMAGE AX_DEFAULT_TASK_IMAGE AX_SNAPSHOTS_BUCKET)
render_manifest() {
  local manifest="${E2E_DIR}/manifests/$1"
  if command -v envsubst > /dev/null 2>&1; then
    envsubst "$(printf '${%s} ' "${MANIFEST_VARS[@]}")" < "${manifest}"
  else
    local sed_args=() var
    for var in "${MANIFEST_VARS[@]}"; do
      sed_args+=(-e "s|\${${var}}|${!var:-}|g")
    done
    sed "${sed_args[@]}" "${manifest}"
  fi
}

# resolve_commit prints the commit SHA that ref names in the git repo at dir,
# trying the ref as-is, as a vX.Y.Z tag, and as a release branch, both locally
# and on origin. Returns non-zero if nothing matches.
resolve_commit() {
  local dir="$1" ref="$2" candidate
  for candidate in "${ref}" "v${ref}" "release-${ref}" "origin/${ref}" "origin/v${ref}" "origin/release-${ref}"; do
    if git -C "${dir}" rev-parse --verify --quiet "${candidate}^{commit}" 2> /dev/null; then
      return 0
    fi
  done
  return 1
}

# resolve_substrate_dir prints a directory containing substrate checked out at
# SUBSTRATE_REF. It never modifies the HEAD of a user-provided checkout: if
# HEAD differs from the requested ref, a detached worktree under .tmp is used.
resolve_substrate_dir() {
  local base_dir="${SUBSTRATE_DIR:-}"
  if [[ -n "${base_dir}" ]]; then
    base_dir="$(cd "${base_dir}" && pwd)"
  elif [[ -d "${REPO_ROOT}/../substrate/.git" ]]; then
    base_dir="$(cd "${REPO_ROOT}/../substrate" && pwd)"
  fi

  if [[ -z "${base_dir}" ]]; then
    base_dir="${TMP_DIR}/substrate"
    if [[ ! -d "${base_dir}/.git" ]]; then
      log "Cloning substrate from ${SUBSTRATE_REPO}..."
      git clone --quiet "${SUBSTRATE_REPO}" "${base_dir}" >&2
    fi
  fi
  [[ -e "${base_dir}/.git" ]] || { echo "SUBSTRATE_DIR=${base_dir} is not a git checkout" >&2; return 1; }

  local target_sha
  if [[ "${base_dir}" == "${TMP_DIR}/substrate" ]] &&
    git -C "${base_dir}" show-ref --verify --quiet "refs/remotes/origin/${SUBSTRATE_REF}"; then
    # Refresh branches in the managed clone without moving its HEAD or a user checkout.
    log "Fetching branch ${SUBSTRATE_REF} in ${base_dir}..."
    git -C "${base_dir}" fetch --quiet origin "+refs/heads/${SUBSTRATE_REF}:refs/remotes/origin/${SUBSTRATE_REF}" >&2 || return 1
    target_sha="$(git -C "${base_dir}" rev-parse "refs/remotes/origin/${SUBSTRATE_REF}^{commit}")"
  elif ! target_sha="$(resolve_commit "${base_dir}" "${SUBSTRATE_REF}")"; then
    log "Fetching ${SUBSTRATE_REF} in ${base_dir}..."
    git -C "${base_dir}" fetch --quiet --tags origin "${SUBSTRATE_REF}" >&2 ||
      git -C "${base_dir}" fetch --quiet --all --tags >&2
    target_sha="$(resolve_commit "${base_dir}" "${SUBSTRATE_REF}")" ||
      { echo "cannot resolve SUBSTRATE_REF=${SUBSTRATE_REF} in ${base_dir}" >&2; return 1; }
  fi

  if [[ "$(git -C "${base_dir}" rev-parse HEAD)" == "${target_sha}" ]]; then
    printf '%s\n' "${base_dir}"
    return 0
  fi

  local wt_dir="${TMP_DIR}/substrate-worktree-${target_sha:0:12}"
  if [[ ! -d "${wt_dir}" ]]; then
    log "Creating detached substrate worktree at ${SUBSTRATE_REF} (${target_sha:0:12})..."
    git -C "${base_dir}" worktree prune >&2 || true
    git -C "${base_dir}" worktree add --quiet --detach "${wt_dir}" "${target_sha}" >&2
  fi
  # Share substrate's tool cache with the worktree to avoid re-downloading tools.
  if [[ -d "${base_dir}/.cache" && ! -e "${wt_dir}/.cache" && ! -L "${wt_dir}/.cache" ]]; then
    ln -s "${base_dir}/.cache" "${wt_dir}/.cache"
  fi
  printf '%s\n' "${wt_dir}"
}

# uninstall_substrate removes Substrate and the cluster-scoped resources it
# owns so that a different Substrate version can be installed cleanly.
uninstall_substrate() {
  local ax_ns="${AX_SYSTEM_NAMESPACE}"
  k -n "${ax_ns}" delete workerpool ax-workers --ignore-not-found --timeout=30s > /dev/null 2>&1 ||
    k -n "${ax_ns}" patch workerpool ax-workers --type=merge -p '{"metadata":{"finalizers":null}}' > /dev/null 2>&1 || true
  k delete validatingadmissionpolicybinding,validatingadmissionpolicy -l app.kubernetes.io/part-of=ate --ignore-not-found > /dev/null 2>&1 || true
  k delete validatingadmissionpolicybinding,validatingadmissionpolicy sandboxconfig-assets --ignore-not-found > /dev/null 2>&1 || true
  if ! k delete sandboxconfig --all --ignore-not-found --timeout=30s > /dev/null 2>&1; then
    local configs
    configs=$(k get sandboxconfig -o name 2> /dev/null || true)
    if [[ -n "${configs}" ]]; then
      echo "${configs}" | xargs kubectl --kubeconfig "${KUBECONFIG}" --context "${KUBE_CONTEXT}" patch --type=merge -p '{"metadata":{"finalizers":null}}' > /dev/null 2>&1 || true
    fi
  fi
  local crds
  crds=$(k get crd -o name 2> /dev/null | grep '\.ate\.dev$' || true)
  if [[ -n "${crds}" ]]; then
    echo "${crds}" | xargs kubectl --kubeconfig "${KUBECONFIG}" --context "${KUBE_CONTEXT}" delete --ignore-not-found > /dev/null 2>&1 || true
  fi
  k delete namespace ate-system --ignore-not-found --wait=true > /dev/null 2>&1 || true
  k label nodes --all ate.dev/substrate-version- > /dev/null 2>&1 || true
}

# build_and_push builds an image, pushes it to the local registry, and prints
# its digest reference.
build_and_push() {
  local tag="$1"
  shift
  docker build -q -t "${tag}" "$@" "${REPO_ROOT}" > /dev/null
  docker push -q "${tag}" > /dev/null
  docker inspect --format='{{index .RepoDigests 0}}' "${tag}"
}

SUBSTRATE_WORK_DIR="$(resolve_substrate_dir)"
SUBSTRATE_COMMIT="$(git -C "${SUBSTRATE_WORK_DIR}" rev-parse HEAD)"
log "Using Substrate checkout at ${SUBSTRATE_WORK_DIR} (ref=${SUBSTRATE_REF}, commit=${SUBSTRATE_COMMIT:0:12})"

# 1. Ensure the Kind cluster and local registry exist.
if [[ "${RECREATE_KIND:-false}" == "true" ]] || ! kind get clusters 2> /dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
  log "Creating Kind cluster '${KIND_CLUSTER_NAME}' and registry '${REGISTRY_ADDR}'..."
  (cd "${SUBSTRATE_WORK_DIR}" && ./hack/create-kind-cluster.sh)
else
  log "Reusing Kind cluster '${KIND_CLUSTER_NAME}' (set RECREATE_KIND=true to recreate)."
fi
kind export kubeconfig --name "${KIND_CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}"

# 2. Install Substrate unless the requested commit is already installed.
INSTALLED_SUBSTRATE_COMMIT="$(k get namespace ate-system -o jsonpath='{.metadata.annotations.substrate-commit}' 2> /dev/null || true)"
if [[ "${FORCE_INSTALL_SUBSTRATE:-false}" == "true" ]] ||
  [[ "${INSTALLED_SUBSTRATE_COMMIT}" != "${SUBSTRATE_COMMIT}" ]] ||
  ! k -n ate-system get deployment ate-api-server > /dev/null 2>&1; then
  log "Installing Substrate (ate-system) at ${SUBSTRATE_COMMIT:0:12}..."
  if [[ -n "${INSTALLED_SUBSTRATE_COMMIT}" ]] || k get namespace ate-system > /dev/null 2>&1; then
    uninstall_substrate
  fi

  install_args=(--deploy-ate-system --rollout-timeout="${SUBSTRATE_ROLLOUT_TIMEOUT:-3m}")
  # Older Substrate versions do not have --credential-provider.
  if grep -q -- "--credential-provider" "${SUBSTRATE_WORK_DIR}/hack/install-ate.sh" 2> /dev/null; then
    install_args+=(--credential-provider='{"enabled":false}')
  fi
  (cd "${SUBSTRATE_WORK_DIR}" && ./hack/install-ate-kind.sh "${install_args[@]}")
  k annotate namespace ate-system "substrate-commit=${SUBSTRATE_COMMIT}" --overwrite > /dev/null
else
  log "Substrate ${SUBSTRATE_COMMIT:0:12} already installed; waiting for ate-api-server..."
  k -n ate-system wait --for=condition=Available deployment/ate-api-server --timeout=180s
fi

# 3. Deploy the AX namespace and gVisor WorkerPool.
log "Deploying namespace '${AX_SYSTEM_NAMESPACE}' and WorkerPool (${AX_WORKER_REPLICAS} gVisor replicas)..."
render_manifest namespace.yaml | k apply -f -
(
  cd "${SUBSTRATE_WORK_DIR}"
  render_manifest workerpool.yaml |
    KO_DOCKER_REPO="${REGISTRY_ADDR}" ./hack/run-tool.sh ko resolve --platform="linux/${GOARCH}" -f -
) | k apply -f -

log "Waiting for ax-workers to become Ready..."
for _ in $(seq 1 60); do
  k -n "${AX_SYSTEM_NAMESPACE}" get deployment ax-workers > /dev/null 2>&1 && break
  sleep 2
done
k -n "${AX_SYSTEM_NAMESPACE}" rollout status deployment/ax-workers --timeout=180s

# 4. Build AX binaries and images. ax-server and ax-task-runner run in the
#    cluster (linux); the ax CLI runs on the host.
log "Building ax (host), ax-server and ax-task-runner (linux/${GOARCH})..."
(
  cd "${REPO_ROOT}"
  mkdir -p "bin/linux_${GOARCH}"
  CGO_ENABLED=0 go build -o "${TMP_DIR}/bin/ax" ./cmd/ax
  CGO_ENABLED=0 GOOS=linux GOARCH="${GOARCH}" go build -o "bin/linux_${GOARCH}/ax-server" ./cmd/ax-server
  CGO_ENABLED=0 GOOS=linux GOARCH="${GOARCH}" go build -o "bin/linux_${GOARCH}/ax-task-runner" ./cmd/ax-task-runner
)

log "Building and pushing images to ${REGISTRY_ADDR}..."
AX_SERVER_IMAGE="$(build_and_push "${REGISTRY_ADDR}/ax-server:e2e" \
  --build-arg TARGETARCH="${GOARCH}" -f "${E2E_DIR}/Dockerfile.server")"
# The E2E task-runner image extends the production image with test fixtures.
docker build -q -t "${REGISTRY_ADDR}/ax-task-runner:base" \
  --build-arg TARGETARCH="${GOARCH}" -f "${REPO_ROOT}/Dockerfile.task-runner" "${REPO_ROOT}" > /dev/null
AX_DEFAULT_TASK_IMAGE="$(build_and_push "${REGISTRY_ADDR}/ax-task-runner:e2e" \
  --build-arg BASE_IMAGE="${REGISTRY_ADDR}/ax-task-runner:base" -f "${E2E_DIR}/Dockerfile.task-runner.e2e")"
export AX_SERVER_IMAGE AX_DEFAULT_TASK_IMAGE
printf '  AX_SERVER_IMAGE=%s\n  AX_DEFAULT_TASK_IMAGE=%s\n' "${AX_SERVER_IMAGE}" "${AX_DEFAULT_TASK_IMAGE}"

# 5. Deploy Postgres and ax-server. Delete existing workloads and Postgres's
#    volume first so every run starts from an empty database.
log "Deploying ax-postgres and ax-server in namespace '${AX_SYSTEM_NAMESPACE}'..."
k -n "${AX_SYSTEM_NAMESPACE}" delete deployment ax-server --ignore-not-found
k -n "${AX_SYSTEM_NAMESPACE}" delete statefulset ax-postgres --ignore-not-found --wait
k -n "${AX_SYSTEM_NAMESPACE}" delete pvc -l app.kubernetes.io/name=ax-postgres --ignore-not-found --wait
k -n "${AX_SYSTEM_NAMESPACE}" create secret generic ax-postgres \
  --from-literal=password="$(openssl rand -hex 24)" --dry-run=client -o yaml | k apply -f -
render_manifest postgres.yaml | k apply -f -
k -n "${AX_SYSTEM_NAMESPACE}" rollout status statefulset/ax-postgres --timeout=180s
render_manifest ax-server.yaml | k apply -f -
k -n "${AX_SYSTEM_NAMESPACE}" rollout status deployment/ax-server --timeout=120s

# 6. Persist the environment for hack/run-e2e.sh and the Go test harness.
cat > "${TMP_DIR}/e2e.env" << EOF
export KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME}"
export KUBECONFIG="${KUBECONFIG}"
export KUBE_CONTEXT="${KUBE_CONTEXT}"
export AX_SYSTEM_NAMESPACE="${AX_SYSTEM_NAMESPACE}"
export AX_BIN="${TMP_DIR}/bin/ax"
export SUBSTRATE_REF="${SUBSTRATE_REF}"
export SUBSTRATE_COMMIT="${SUBSTRATE_COMMIT}"
EOF

log "Kind E2E environment is ready (configuration written to ${TMP_DIR}/e2e.env)."
