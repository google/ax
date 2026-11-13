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

# AX State Machine Prototype Demo (Standalone — no Kind/K8s cluster required):
#   1. Builds bin/ax and bin/ax-server and starts a local ax-server backed by
#      the in-memory store and Agent Substrate control API server.
#   2. Creates a root Task (auto-generating session_id) and a child Task
#      (parentTaskId inheriting the parent's session_id).
#   3. Resumes both tasks (QUEUED -> WORKING) and suspends the child task
#      (WORKING -> SUSPENDING -> SUSPENDED with ADMIN_SUSPENDED).
#   4. Cancels the parent task and demonstrates parent -> child cancellation
#      cascade and terminal state immutability.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

ATESPACE="${ATESPACE:-default}"
SERVER_ADDR="127.0.0.1:18080"
export AX_SERVER="http://${SERVER_ADDR}"
export NO_PROXY="127.0.0.1,localhost"
export no_proxy="127.0.0.1,localhost"

if [[ -t 1 && -z "${NO_COLOR:-}" ]]; then
  BOLD=$'\e[1m'; DIM=$'\e[2m'; CYAN=$'\e[36m'; GREEN=$'\e[32m'; YELLOW=$'\e[33m'; RESET=$'\e[0m'
else
  BOLD=""; DIM=""; CYAN=""; GREEN=""; YELLOW=""; RESET=""
fi

STEP=0
step() {
  STEP=$((STEP + 1))
  printf '\n%s%s━━ %d. %s%s\n\n' "${BOLD}" "${CYAN}" "${STEP}" "$*" "${RESET}"
}

run() {
  printf '%s$ %s%s\n' "${DIM}" "$*" "${RESET}"
  "$@"
}

ok()   { printf '%s✔ %s%s\n' "${GREEN}" "$*" "${RESET}"; }
note() { printf '%s%s%s\n' "${YELLOW}" "$*" "${RESET}"; }

ax() { ./bin/ax --server "${AX_SERVER}" "$@"; }

printf '\n%s🚀 AX Task State Machine Prototype Demo%s  %s(inline Substrate calls + session lineage)%s\n' \
  "${BOLD}" "${RESET}" "${DIM}" "${RESET}"

step "Build bin/ax and bin/ax-server and start local control plane"
mkdir -p bin
go build -o bin/ax ./cmd/ax
go build -o bin/ax-server ./cmd/ax-server

./bin/ax-server --addr="${SERVER_ADDR}" --metrics-addr="" --store=memory --substrate-endpoint=memory >/tmp/ax-demo-server.log 2>&1 &
SERVER_PID=$!
DEMO_YAML=$(mktemp /tmp/ax-sm-demo-XXXXXX.yaml)
trap 'kill "${SERVER_PID}" >/dev/null 2>&1 || true; rm -f "${DEMO_YAML}"' EXIT

for _ in {1..30}; do
  if curl -sf "${AX_SERVER}/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done
ok "Local ax-server listening at ${AX_SERVER} (PID ${SERVER_PID})"

step "Create a root Task and a child Task (go/ax-task-lineage)"
cat <<YAML > "${DEMO_YAML}"
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: root-triage
  atespace: ${ATESPACE}
spec:
  image: gcr.io/ax-substrate/ate-images/ax-task-runner@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  goal: "Investigate flaky payment test"
  retry:
    maxRetries: 2
---
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: child-fixer
  atespace: ${ATESPACE}
spec:
  image: gcr.io/ax-substrate/ate-images/ax-task-runner@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  parentTaskId: root-triage
  goal: "Author regression fix for payment test"
YAML
printf '%s' "${DIM}"; sed 's/^/    /' "${DEMO_YAML}"; printf '%s\n\n' "${RESET}"
run ax apply -f "${DEMO_YAML}"
echo
note "Notice how root-triage auto-generated a Session ID and child-fixer inherited it:"
run ax describe task root-triage
echo
run ax describe task child-fixer

step "Resume both tasks (QUEUED/SUSPENDED -> WORKING)"
run ax resume task root-triage
run ax resume task child-fixer
echo
run ax get tasks
echo
run ax describe task root-triage

step "Suspend child-fixer (WORKING -> SUSPENDING -> SUSPENDED with ADMIN_SUSPENDED)"
run ax suspend task child-fixer
echo
run ax describe task child-fixer

step "Cancel root-triage and verify cancellation cascades to child-fixer"
run ax cancel task root-triage
echo
run ax get tasks
echo
note "Both root-triage and child-fixer are now in terminal TASK_STATE_CANCELLED (TASK_RESULT_CANCELLED):"
run ax describe task root-triage
echo
run ax describe task child-fixer

step "Verify terminal states are immutable (ResumeTask rejected with FailedPrecondition)"
printf '%s$ ax resume task root-triage%s\n' "${DIM}" "${RESET}"
if ax resume task root-triage 2>&1; then
  echo "unexpected success"
  exit 1
else
  ok "ResumeTask on terminal CANCELLED task rightly rejected with FailedPrecondition"
fi

printf '\n%s🎉 State Machine Prototype Demo complete!%s\n\n' "${BOLD}" "${RESET}"
