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

# AX demo: the full lifecycle of one task in about two minutes.
#
#   1. Declare a Workspace and a Task in YAML and apply them.
#   2. Watch the task come up and the workspace get initialized.
#   3. Look inside the task with `ax ssh`.
#   4. Suspend the task, checkpointing its workspace.
#
# Environment:
#   AX_BIN               path to the ax CLI                        (default: ./bin/ax)
#   ATESPACE             atespace to run the demo in               (default: default)
#   SUBSTRATE_NAMESPACE  namespace Agent Substrate is installed in (default: ate-system)
#   NO_COLOR             set to disable colored output

set -euo pipefail

AX_BIN="${AX_BIN:-./bin/ax}"
SUBSTRATE_NAMESPACE="${SUBSTRATE_NAMESPACE:-ate-system}"

# ax runs the CLI at AX_BIN so the commands below read the way you would type them.
ax() { "${AX_BIN}" "$@"; }
ATESPACE="${ATESPACE:-default}"
TASK_NAME="demo-task"
WORKSPACE_NAME="demo-workspace"
TASK_IMAGE="${AX_TASK_IMAGE:-${AX_IMAGE_REPO:-gcr.io/ax-substrate/ate-images}/ax-task-runner@sha256:464c5a53c68c67e929dbbb5450f1eb41f99b2742efcf42b721c09825a58397f1}"

# ---------------------------------------------------------------------------
# Presentation helpers
# ---------------------------------------------------------------------------

if [[ -t 1 && -z "${NO_COLOR:-}" ]]; then
  BOLD=$'\e[1m'; DIM=$'\e[2m'; CYAN=$'\e[36m'; GREEN=$'\e[32m'; YELLOW=$'\e[33m'; RED=$'\e[31m'; RESET=$'\e[0m'
else
  BOLD=""; DIM=""; CYAN=""; GREEN=""; YELLOW=""; RED=""; RESET=""
fi

STEP=0
step() {
  STEP=$((STEP + 1))
  printf '\n%s%s━━ %d. %s%s\n\n' "${BOLD}" "${CYAN}" "${STEP}" "$*" "${RESET}"
}

# run prints the command the way you would type it, then runs it.
run() {
  printf '%s$ %s%s\n' "${DIM}" "$*" "${RESET}"
  "$@"
}

# in_sandbox runs a shell snippet inside the task over `ax ssh`.
in_sandbox() {
  printf "%s\$ ax ssh %s -- sh -c '%s'%s\n" "${DIM}" "${TASK_NAME}" "$1" "${RESET}"
  ax ssh "${TASK_NAME}" -a "${ATESPACE}" -- sh -c "$1"
}

ok()   { printf '%s✔ %s%s\n' "${GREEN}" "$*" "${RESET}"; }
note() { printf '%s%s%s\n' "${YELLOW}" "$*" "${RESET}"; }
err()  { printf '%s✘ %s%s\n' "${RED}" "$*" "${RESET}"; }

# ---------------------------------------------------------------------------
# Demo
# ---------------------------------------------------------------------------

printf '\n%s🚀 AX demo%s  %sworkspace=%s task=%s atespace=%s%s\n' \
  "${BOLD}" "${RESET}" "${DIM}" "${WORKSPACE_NAME}" "${TASK_NAME}" "${ATESPACE}" "${RESET}"

step "Preflight checks"
if ! command -v "${AX_BIN}" >/dev/null 2>&1 && [[ ! -x "${AX_BIN}" ]]; then
  err "ax CLI not found at '${AX_BIN}'. Build it with 'make build' or point AX_BIN at your binary."
  exit 1
fi
ok "ax CLI found at ${AX_BIN}"
if ! command -v kubectl >/dev/null 2>&1; then
  err "kubectl not found. It is needed to verify that Agent Substrate is installed."
  exit 1
fi
# AX schedules tasks as actors on Agent Substrate; without it every task
# fails at actor creation. Check for its Control API before touching anything.
if ! kubectl get svc api -n "${SUBSTRATE_NAMESPACE}" --request-timeout=10s >/dev/null 2>&1; then
  err "Agent Substrate not detected: no 'api' Service in namespace '${SUBSTRATE_NAMESPACE}'."
  note "Install Agent Substrate first — see the Prerequisites section of the AX README"
  note "or https://github.com/agent-substrate/substrate. If it is installed in a"
  note "different namespace, set SUBSTRATE_NAMESPACE."
  exit 1
fi
ok "Agent Substrate Control API found in namespace ${SUBSTRATE_NAMESPACE}"

step "Clean up any previous demo run"
CLEANED=0
ax delete task "${TASK_NAME}" -a "${ATESPACE}" >/dev/null 2>&1 && { ok "removed old task"; CLEANED=1; } || true
ax delete workspace "${WORKSPACE_NAME}" -a "${ATESPACE}" >/dev/null 2>&1 && { ok "removed old workspace"; CLEANED=1; } || true
(( CLEANED )) || echo "nothing to clean up"

step "Declare a Workspace and a Task"
DEMO_YAML=$(mktemp /tmp/ax-demo-XXXXXX)
trap 'rm -f "${DEMO_YAML}"' EXIT
cat <<YAML > "${DEMO_YAML}"
apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: ${WORKSPACE_NAME}
  atespace: ${ATESPACE}
---
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: ${TASK_NAME}
  atespace: ${ATESPACE}
spec:
  debug: true
  image: "${TASK_IMAGE}"
  workspaces:
    - name: ${WORKSPACE_NAME}
      path: "/workspace"
  env:
    - name: DEMO_ENV
      value: "active"
YAML
printf '%s' "${DIM}"; sed 's/^/    /' "${DEMO_YAML}"; printf '%s\n\n' "${RESET}"
run ax apply -f "${DEMO_YAML}"

step "Resume the task"
note "New tasks are created Suspended by default. Resuming creates the worker on Agent Substrate and initializes /workspace."
run ax resume task "${TASK_NAME}" -a "${ATESPACE}"
ok "${TASK_NAME} is Running and Ready"
echo
run ax get tasks -a "${ATESPACE}"
echo
run ax describe task "${TASK_NAME}" -a "${ATESPACE}"

step "Look inside the task with ax ssh"
in_sandbox 'ls -la /workspace'
echo
in_sandbox 'echo "DEMO_ENV=$DEMO_ENV"; echo "AX_METADATA_URL=$AX_METADATA_URL"'
echo
note "The runner serves the task's own spec back to it over HTTP:"
in_sandbox 'curl -s "$AX_METADATA_URL/metadata/v1alpha1/ax/task" | head -20'

step "Suspend the task"
run ax suspend task "${TASK_NAME}" -a "${ATESPACE}"
ok "${TASK_NAME} is Suspended. The workspace has been checkpointed and the sandbox is gone."
echo
run ax get tasks -a "${ATESPACE}"

printf '\n%s🎉 Demo complete.%s %s is left Suspended. Pick it back up or clean up with:\n\n' "${BOLD}" "${RESET}" "${TASK_NAME}"
printf '    ax resume task %s -a %s\n' "${TASK_NAME}" "${ATESPACE}"
printf '    ax ssh %s -a %s\n' "${TASK_NAME}" "${ATESPACE}"
printf '    ax delete task %s -a %s\n\n' "${TASK_NAME}" "${ATESPACE}"
