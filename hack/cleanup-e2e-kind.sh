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

# Deletes the Kind cluster created for AX E2E tests.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
TMP_DIR="${REPO_ROOT}/.tmp"

if [[ -z "${KIND_CLUSTER_NAME:-}" && -f "${TMP_DIR}/e2e.env" ]]; then
  # shellcheck disable=SC1091
  source "${TMP_DIR}/e2e.env"
fi

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-ax-e2e}"
echo "==> Deleting Kind cluster '${KIND_CLUSTER_NAME}'..."
kind delete cluster --name "${KIND_CLUSTER_NAME}" || true
rm -f "${TMP_DIR}/kubeconfig-${KIND_CLUSTER_NAME}" "${TMP_DIR}/e2e.env"
