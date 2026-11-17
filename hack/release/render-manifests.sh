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

# Renders the release install manifests.
#
#   hack/release/render-manifests.sh [out-dir]
#
# Reads out-dir/images.env (written by publish-images.sh; default out-dir is
# bin/release) and writes:
#
#   ax-server.yaml  deploy/ax-server.yaml with both images pinned by digest and
#                   AX_RUNNER_VOLUME_IMAGE empty: the runner volume (#619, #620)
#                   is not released yet, so tasks run the runner baked into
#                   their image
#   postgres.yaml   deploy/postgres.yaml, unchanged
#
# Fails if a ${...} placeholder is left in a rendered manifest.

set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
OUT="${1:-${ROOT}/bin/release}"

# shellcheck disable=SC1091
source "${OUT}/images.env"
for v in AX_SERVER_IMAGE AX_DEFAULT_TASK_IMAGE; do
  if [[ ! "${!v:-}" =~ @sha256:[0-9a-f]{64}$ ]]; then
    echo "error: ${v} in ${OUT}/images.env is not pinned by digest: '${!v:-}'" >&2
    exit 1
  fi
done

sed -e "s|\${AX_SERVER_IMAGE}|${AX_SERVER_IMAGE}|g" \
  -e "s|\${AX_DEFAULT_TASK_IMAGE}|${AX_DEFAULT_TASK_IMAGE}|g" \
  -e "s|\${AX_RUNNER_VOLUME_IMAGE}||g" \
  "${ROOT}/deploy/ax-server.yaml" > "${OUT}/ax-server.yaml"
cp "${ROOT}/deploy/postgres.yaml" "${OUT}/postgres.yaml"

for f in ax-server.yaml postgres.yaml; do
  # shellcheck disable=SC2016  # matches a literal ${
  if grep -n '\${' "${OUT}/${f}"; then
    echo "error: unrendered placeholder in ${OUT}/${f}" >&2
    exit 1
  fi
done
echo "==> Rendered ${OUT}/ax-server.yaml and ${OUT}/postgres.yaml"
