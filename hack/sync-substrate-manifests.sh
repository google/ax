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

# Exports deterministic, digest-pinned Agent Substrate manifests for `ax local`
# at the exact version pinned in go.mod. Run this script whenever bumping
# go.ate.dev/substrate in go.mod.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
OUT_DIR="${REPO_ROOT}/internal/install/substrate/manifests"
IMAGE_REPO="${SUBSTRATE_IMAGE_REPO:-ghcr.io/agent-substrate/substrate}"

VERSION="$(cd "${REPO_ROOT}" && GOTOOLCHAIN=auto go list -m -f '{{.Version}}' go.ate.dev/substrate)"
if [[ -z "${VERSION}" ]]; then
  echo "error: could not determine go.ate.dev/substrate version from go.mod" >&2
  exit 1
fi

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

extract_source() {
  local src_dir="${TMP_DIR}/src"
  mkdir -p "${src_dir}"

  local candidate
  for candidate in "${SUBSTRATE_DIR:-}" "${REPO_ROOT}/../substrate" "${REPO_ROOT}/../substrate/substrate"; do
    if [[ -n "${candidate}" && -d "${candidate}/.git" ]]; then
      if git -C "${candidate}" rev-parse --verify --quiet "${VERSION}^{commit}" >/dev/null 2>&1; then
        echo "==> Extracting Substrate ${VERSION} from local repo ${candidate}..." >&2
        git -C "${candidate}" archive "${VERSION}" | tar -x -C "${src_dir}"
        printf '%s\n' "${src_dir}"
        return 0
      fi
    fi
  done

  local tarball_url="https://github.com/agent-substrate/substrate/archive/refs/tags/${VERSION}.tar.gz"
  echo "==> Downloading Substrate ${VERSION} archive from ${tarball_url}..." >&2
  curl -fsSL "${tarball_url}" | tar -xz --strip-components=1 -C "${src_dir}"
  printf '%s\n' "${src_dir}"
}

resolve_ghcr_digest() {
  local name="$1"
  local repo_path="${IMAGE_REPO#ghcr.io/}/${name}"
  local token
  token="$(curl -fsSL "https://ghcr.io/token?service=ghcr.io&scope=repository:${repo_path}:pull" | jq -r '.token')"
  local digest
  digest="$(
    curl -fsSI \
      -H "Authorization: Bearer ${token}" \
      -H 'Accept: application/vnd.oci.image.index.v1+json' \
      -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
      -H 'Accept: application/vnd.docker.distribution.manifest.list.v2+json' \
      -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' \
      "https://ghcr.io/v2/${repo_path}/manifests/${VERSION}" |
      tr -d '\r' |
      awk 'tolower($1) == "docker-content-digest:" { print $2; found = 1 } END { exit !found }'
  )"
  printf '%s/%s:%s@%s' "${IMAGE_REPO}" "${name}" "${VERSION}" "${digest}"
}

SRC_DIR="$(extract_source)"

echo "==> Resolving digest-pinned Substrate ${VERSION} images from ${IMAGE_REPO}..." >&2
declare -A PINNED_IMAGES
for img in \
  podcertcontroller \
  ateapi \
  atecontroller \
  atelet \
  atenet \
  ate-idp-server \
  kubernetes-secrets \
  objectstore-plugin \
  ateworker-gvisor \
  envoy-dataplane; do
  PINNED_IMAGES["${img}"]="$(resolve_ghcr_digest "${img}")"
  echo "  ${img} -> ${PINNED_IMAGES[${img}]}" >&2
done

replace_images() {
  sed \
    -e "s|ko://go.ate.dev/substrate/cmd/podcertcontroller|${PINNED_IMAGES[podcertcontroller]}|g" \
    -e "s|ko://go.ate.dev/substrate/cmd/ateapi|${PINNED_IMAGES[ateapi]}|g" \
    -e "s|ko://go.ate.dev/substrate/cmd/atecontroller|${PINNED_IMAGES[atecontroller]}|g" \
    -e "s|ko://go.ate.dev/substrate/cmd/atelet|${PINNED_IMAGES[atelet]}|g" \
    -e "s|ko://go.ate.dev/substrate/cmd/atenet|${PINNED_IMAGES[atenet]}|g" \
    -e "s|ko://go.ate.dev/substrate/cmd/ate-idp-server|${PINNED_IMAGES[ate-idp-server]}|g" \
    -e "s|ko://go.ate.dev/substrate/cmd/credential-provider/kubernetes-secrets|${PINNED_IMAGES[kubernetes-secrets]}|g" \
    -e "s|ko://go.ate.dev/substrate/cmd/objectstore-plugin|${PINNED_IMAGES[objectstore-plugin]}|g" \
    -e "s|ko://go.ate.dev/substrate/cmd/ateworker-gvisor|${PINNED_IMAGES[ateworker-gvisor]}|g" \
    -e "s|\${ENVOY_DATAPLANE_IMAGE}|${PINNED_IMAGES[envoy-dataplane]}|g"
}

mkdir -p "${OUT_DIR}"
rm -f "${OUT_DIR}"/*.yaml "${OUT_DIR}/version.txt"
printf '%s\n' "${VERSION}" > "${OUT_DIR}/version.txt"

echo "==> Rendering manifests into ${OUT_DIR}..." >&2

{
  first=1
  for crd in "${SRC_DIR}/manifests/ate-install/generated"/*.yaml; do
    if [[ "${first}" -eq 0 ]]; then
      printf '\n---\n'
    fi
    cat "${crd}"
    first=0
  done
} > "${OUT_DIR}/01-crds.yaml"

replace_images < "${SRC_DIR}/manifests/ate-install/pod-certificate-controller.yaml" \
  > "${OUT_DIR}/02-podcert-controller.yaml"

{
  cat "${SRC_DIR}/manifests/ate-install/sandboxconfig-validation.yaml"
  printf '\n---\n'
  cat "${SRC_DIR}/manifests/ate-install/sandboxconfig-gvisor.yaml"
} > "${OUT_DIR}/03-sandboxconfig.yaml"

kubectl kustomize --load-restrictor=LoadRestrictionsNone \
  "${SRC_DIR}/manifests/ate-install/kind/postgres" > "${OUT_DIR}/04-postgres.yaml"

# Render ate-system Kind overlay and set ate-idp-server --issuer=https://idp.ate-system.svc.
STAGE_IDP="${TMP_DIR}/stage-ate-system"
mkdir -p "${STAGE_IDP}"
kubectl kustomize --load-restrictor=LoadRestrictionsNone \
  "${SRC_DIR}/manifests/ate-install/kind" | replace_images > "${STAGE_IDP}/base.yaml"
cat > "${STAGE_IDP}/kustomization.yaml" <<'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - base.yaml
patches:
  - target:
      group: apps
      version: v1
      kind: Deployment
      name: ate-idp-server
      namespace: ate-system
    patch: |-
      - op: add
        path: /spec/template/spec/containers/0/args/-
        value: "--issuer=https://idp.ate-system.svc"
EOF
kubectl kustomize "${STAGE_IDP}" > "${OUT_DIR}/05-ate-system.yaml"

replace_images < "${SRC_DIR}/manifests/egress-credential-injection/k8s-credential-provider.yaml" \
  > "${OUT_DIR}/06-credprovider.yaml"

awk '
  /^[[:space:]]*#ATE_EGRESS_INJECT_FLAGS/ {
    match($0, /^[[:space:]]*/)
    indent = substr($0, RSTART, RLENGTH)
    print indent "- --credential-provider-name=k8s.io"
    print indent "- --credential-provider-address=k8s-credential-provider.ate-system.svc.cluster.local:50051"
    print indent "- --credential-provider-server-name=k8s-credential-provider.ate-system.svc"
    print indent "- --credential-provider-ca-file=/run/servicedns.podcert.ate.dev/trust-bundle.pem"
    print indent "- --credential-provider-client-cert=/run/podidentity.podcert.ate.dev/credential-bundle.pem"
    next
  }
  { print }
' "${SRC_DIR}/manifests/ate-install/atenet-egress.yaml" | replace_images \
  > "${OUT_DIR}/07-atenet-egress.yaml"

cat > "${OUT_DIR}/08-workerpool.yaml" <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: ate-workers
---
apiVersion: ate.dev/v1
kind: WorkerPool
metadata:
  name: ax-workers
  namespace: ate-workers
spec:
  replicas: \${AX_WORKER_REPLICAS}
  workerImage: ${PINNED_IMAGES[ateworker-gvisor]}
  sandboxClasses:
  - name: gvisor
    configRef:
      name: gvisor-default
EOF

# Sanity check: ensure no unresolved ko:// or ${ENVOY_DATAPLANE_IMAGE} remain.
if grep -rn 'ko://go\.ate\.dev/substrate\|\${ENVOY_DATAPLANE_IMAGE}\|#ATE_EGRESS_INJECT_FLAGS' "${OUT_DIR}"/*.yaml; then
  echo "error: unresolved image or marker left in ${OUT_DIR}" >&2
  exit 1
fi

echo "==> Successfully synced Substrate ${VERSION} manifests into ${OUT_DIR}" >&2
