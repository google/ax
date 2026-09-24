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
#   AX_BIN    path to the ax CLI            (default: ./bin/ax)
#   ATESPACE  atespace to run the demo in   (default: default)
#   NO_COLOR  set to disable colored output

set -euo pipefail

AX_BIN="${AX_BIN:-./bin/ax}"

# ax runs the CLI at AX_BIN so the commands below read the way you would type them.
ax() { "${AX_BIN}" "$@"; }
ATESPACE="${ATESPACE:-default}"
TASK_NAME="demo-task"
WORKSPACE_NAME="demo-workspace"
TASK_IMAGE="${AX_TASK_IMAGE:-${AX_IMAGE_REPO:-gcr.io/dberkov-gke-dev3}/ax-task-runner@sha256:127dbe6650f2b93e5af793a9d7995ce0cf70c0f37ffb4c696154d3cc1a32f8bd}"

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
task_field() {
  ax describe task "${TASK_NAME}" -a "${ATESPACE}" 2>/dev/null | awk -v key="$1" '$1 == key {print $2}'
}

# wait_for PHASE [READY] polls the task until it reaches PHASE (and Ready=READY
# when given), printing a dot per poll and the elapsed time when it gets there.
wait_for() {
  local want_phase="$1" want_ready="${2:-}" timeout="${3:-180}"
  local start phase ready elapsed
  start=$(date +%s)
  printf '%swaiting for Phase=%s' "${DIM}" "${want_phase}"
  [[ -n "${want_ready}" ]] && printf ' Ready=%s' "${want_ready}"
  printf '%s ' "${RESET}"
  while :; do
    phase=$(task_field "Phase:")
    ready=$(task_field "Ready")
    if [[ "${phase}" == "${want_phase}" && ( -z "${want_ready}" || "${ready}" == "${want_ready}" ) ]]; then
      elapsed=$(( $(date +%s) - start ))
      printf ' %s%ds%s\n' "${GREEN}" "${elapsed}" "${RESET}"
      return 0
    fi
    if [[ "${phase}" == "Failed" && "${want_phase}" != "Failed" ]]; then
      printf '\n'
      err "Task entered Failed phase! Last seen Ready=${ready:-?}"
      ax describe task "${TASK_NAME}" -a "${ATESPACE}" || true
      return 1
    fi
    if (( $(date +%s) - start > timeout )); then
      printf '\n'
      note "Gave up after ${timeout}s. Last seen Phase=${phase:-?} Ready=${ready:-?}"
      ax describe task "${TASK_NAME}" -a "${ATESPACE}" || true
      return 1
    fi
    printf '.'
    sleep 2
  done
}

# ---------------------------------------------------------------------------
# Demo
# ---------------------------------------------------------------------------

printf '\n%s🚀 AX demo%s  %sworkspace=%s task=%s atespace=%s%s\n' \
  "${BOLD}" "${RESET}" "${DIM}" "${WORKSPACE_NAME}" "${TASK_NAME}" "${ATESPACE}" "${RESET}"

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

step "Watch the task come up"
note "The controller creates an actor on Agent Substrate and initializes /workspace."
wait_for "Running" "True"
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
wait_for "Suspended"
ok "${TASK_NAME} is Suspended. The workspace has been checkpointed and the sandbox is gone."
echo
run ax get tasks -a "${ATESPACE}"

printf '\n%s🎉 Demo complete.%s %s is left Suspended. Pick it back up or clean up with:\n\n' "${BOLD}" "${RESET}" "${TASK_NAME}"
printf '    ax resume task %s -a %s\n' "${TASK_NAME}" "${ATESPACE}"
printf '    ax ssh %s -a %s\n' "${TASK_NAME}" "${ATESPACE}"
printf '    ax delete task %s -a %s\n\n' "${TASK_NAME}" "${ATESPACE}"
