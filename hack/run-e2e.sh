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

# Runs the AX Go E2E test suite against an already-provisioned Kind cluster.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
TMP_DIR="${REPO_ROOT}/.tmp"

if [[ -f "${TMP_DIR}/e2e.env" ]]; then
  # shellcheck disable=SC1091
  source "${TMP_DIR}/e2e.env"
fi

# Rebuild the host ax CLI so tests always exercise the current source, unless
# AX_BIN points at a custom binary.
DEFAULT_AX_BIN="${TMP_DIR}/bin/ax"
export AX_BIN="${AX_BIN:-${DEFAULT_AX_BIN}}"
if [[ "${AX_BIN}" == "${DEFAULT_AX_BIN}" ]]; then
  mkdir -p "$(dirname "${AX_BIN}")"
  (cd "${REPO_ROOT}" && CGO_ENABLED=0 go build -o "${AX_BIN}" ./cmd/ax)
fi

cd "${REPO_ROOT}"
exec go test -v -count=1 -timeout "${E2E_TIMEOUT:-30m}" ./test/e2e/... -args --e2e "$@"
