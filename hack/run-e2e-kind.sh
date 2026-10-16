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

# Provisions a Kind cluster with Substrate (at SUBSTRATE_REF, default: go.mod version)
# and AX, then runs the AX E2E test suite.
#
# Set SKIP_SETUP=true to reuse an already set up cluster.
# Set CLEANUP_KIND=true to delete the Kind cluster on exit.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [[ "${CLEANUP_KIND:-false}" == "true" ]]; then
  trap '"${SCRIPT_DIR}/cleanup-e2e-kind.sh"' EXIT
fi

if [[ "${SKIP_SETUP:-false}" != "true" ]]; then
  export KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-ax-e2e}"
  "${SCRIPT_DIR}/setup-e2e-kind.sh"
fi

"${SCRIPT_DIR}/run-e2e.sh" "$@"
